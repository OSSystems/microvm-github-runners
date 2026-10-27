# Runs GitHub Actions jobs in throwaway microVMs. microvm-scaleset boots VMs
# from one pool shared by every organization while the host has room, each with a fresh disk and a
# just-in-time runner config, so the GitHub App key never enters a VM and
# nothing outlives the job.
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.microvm-github-runners;
  hostAddress = "${cfg.network.subnet}.1";
  nixDaemonPort = 5000;
  package = pkgs.callPackage ../package.nix { };

  slots = builtins.genList (i: {
    name = "${cfg.name}-${toString (i + 1)}";
    index = i + 1;
  }) cfg.poolSize;
  slotNames = map (s: s.name) slots;
in
{
  imports = [
    (lib.mkChangedOptionModule
      [ "services" "microvm-github-runners" "url" ]
      [ "services" "microvm-github-runners" "urls" ]
      (config: [ config.services.microvm-github-runners.url ])
    )
    (lib.mkChangedOptionModule
      [ "services" "microvm-github-runners" "memOvercommit" ]
      [ "services" "microvm-github-runners" "memCommitRatio" ]
      (config: 1.0 + config.services.microvm-github-runners.memOvercommit)
    )
  ];

  options.services.microvm-github-runners = {
    enable = lib.mkEnableOption "GitHub Actions runners in throwaway microVMs";

    urls = lib.mkOption {
      type = lib.types.nonEmptyListOf lib.types.str;
      example = [
        "https://github.com/my-org"
        "https://github.com/my-other-org"
      ];
      description = ''
        Organizations to register a scale set with. They all share the VM
        pool, so jobs of a busy organization use the VMs idle ones leave.
      '';
    };
    name = lib.mkOption {
      type = lib.types.str;
      description = "Scale set name in every organization; also prefixes the VM names.";
    };
    labels = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ cfg.name ];
      defaultText = lib.literalExpression "[ name ]";
      description = "Labels workflows use in `runs-on`.";
    };
    githubApp = {
      id = lib.mkOption {
        type = lib.types.str;
        description = "GitHub App ID.";
      };
      privateKeyFile = lib.mkOption {
        type = lib.types.path;
        description = "GitHub App private key. Read by the controller only; never shared with VMs.";
      };
    };

    poolSize = lib.mkOption {
      type = lib.types.ints.positive;
      default = 8;
      description = "Most VMs that may run at once, across all organizations; how many do depends on load.";
    };
    vm = {
      memGiB = lib.mkOption {
        type = lib.types.ints.positive;
        default = 10;
      };
      vcpu = lib.mkOption {
        type = lib.types.ints.positive;
        default = 8;
      };
      cpuWeight = lib.mkOption {
        type = lib.types.ints.between 1 10000;
        default = 100;
        description = "Relative CPU share of each VM under contention. Idle CPU remains available for bursts.";
      };
      cpuQuotaPercent = lib.mkOption {
        type = lib.types.nullOr lib.types.ints.positive;
        default = null;
        example = 400;
        description = "Optional CPU ceiling for each VM process; 400 permits four CPUs. Host-side Nix builds run separately.";
      };
      diskGiB = lib.mkOption {
        type = lib.types.ints.positive;
        default = 100;
        description = "Sparse disk recreated for every job.";
      };
    };
    maxLoadPerCpu = lib.mkOption {
      type = lib.types.float;
      default = 1.0;
      description = "Take no new jobs above this 1-minute load average per CPU.";
    };
    memReserveGiB = lib.mkOption {
      type = lib.types.ints.unsigned;
      default = 8;
      description = "Memory always left for the host when deciding to boot another VM.";
    };
    memCommitRatio = lib.mkOption {
      type = lib.types.float;
      default = 1.0;
      example = 1.3;
      description = ''
        Maximum total boot memory divided by host RAM after memReserveGiB.
        1.0 permits 100%, 1.3 permits 130%, and 2.0 permits 200%.
        Available host memory and pending VM growth also limit new boots.
        Ballooning does not reduce this nominal commitment.
      '';
    };
    balloon = {
      enable = lib.mkEnableOption "pressure-driven guest memory reclamation";
      floorGiB = lib.mkOption {
        type = lib.types.ints.positive;
        default = lib.min 8 cfg.vm.memGiB;
        description = "Lowest guest allocation during reclamation; the boot ceiling remains vm.memGiB.";
      };
      headroomGiB = lib.mkOption {
        type = lib.types.ints.positive;
        default = 2;
        description = "Available memory to retain inside each guest when reclaiming cache.";
      };
      canarySlot = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = "Restrict reclamation to one slot during rollout; null enables it across the shared pool.";
      };
    };

    network = {
      bridge = lib.mkOption {
        type = lib.types.str;
        default = "microbr";
      };
      subnet = lib.mkOption {
        type = lib.types.str;
        default = "10.231.0";
        description = "First three octets of the /24 the VMs live in.";
      };
      externalInterface = lib.mkOption {
        type = lib.types.str;
        description = "Interface the VMs are NATed out of.";
      };
    };

    guestModules = lib.mkOption {
      type = lib.types.listOf lib.types.deferredModule;
      default = [ ];
      description = ''
        Extra NixOS modules for every VM, e.g. to set
        `microvm-github-runner.packages` or add services jobs need.
      '';
    };
    guestSpecialArgs = lib.mkOption {
      type = lib.types.attrs;
      default = { };
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.memCommitRatio >= 1.0;
        message = "microvm-github-runners.memCommitRatio must be at least 1.0.";
      }
      {
        assertion = cfg.balloon.floorGiB <= cfg.vm.memGiB;
        message = "microvm-github-runners.balloon.floorGiB exceeds vm.memGiB.";
      }
      {
        assertion = cfg.balloon.canarySlot == null || builtins.elem cfg.balloon.canarySlot slotNames;
        message = "microvm-github-runners.balloon.canarySlot must name a configured slot.";
      }
    ];
    microvm.vms = lib.listToAttrs (
      map (slot: {
        inherit (slot) name;
        value = {
          # microvm-scaleset starts VMs on demand; a deploy must not kill a
          # running job, the next boot picks up the new system.
          autostart = false;
          restartIfChanged = false;
          specialArgs = cfg.guestSpecialArgs;
          config = {
            imports = [ ./guest.nix ] ++ cfg.guestModules;
            microvm-github-runner = {
              inherit (slot) name;
              inherit hostAddress nixDaemonPort;
              inherit (cfg.vm) vcpu diskGiB;
              address = "${cfg.network.subnet}.${toString (10 + slot.index)}";
              mac = "02:00:0a:e7:00:${lib.fixedWidthString 2 "0" (lib.toHexString slot.index)}";
              tap = "ghrunner${toString slot.index}";
              cid = 2 + slot.index;
              mem = cfg.vm.memGiB * 1024;
            };
          };
        };
      }) slots
    );

    users = {
      users.microvm-nix = {
        isSystemUser = true;
        group = "microvm-nix";
      };
      groups.microvm-nix = { };
    };
    # VMs build through the host daemon as this untrusted user, so builds and
    # their cache land in the host store while VMs only see it read-only.
    nix.settings.allowed-users = [ "microvm-nix" ];

    systemd = {
      services = {
        microvm-scaleset = {
          wantedBy = [ "multi-user.target" ];
          wants = [ "network-online.target" ];
          after = [ "network-online.target" ];
          path = [ config.systemd.package ];
          serviceConfig = {
            ExecStart = lib.escapeShellArgs (
              [ (lib.getExe package) ]
              ++ map (url: "--url=${url}") cfg.urls
              ++ [
                "--name=${cfg.name}"
                "--labels=${lib.concatStringsSep "," cfg.labels}"
                "--app-id=${cfg.githubApp.id}"
                "--private-key-file=${cfg.githubApp.privateKeyFile}"
                "--slots=${lib.concatStringsSep "," slotNames}"
                "--vm-mem=${toString cfg.vm.memGiB}"
                "--mem-reserve=${toString cfg.memReserveGiB}"
                "--mem-commit-ratio=${toString cfg.memCommitRatio}"
                "--state-dir=${config.microvm.stateDir}"
                "--balloon-enable=${lib.boolToString cfg.balloon.enable}"
                "--balloon-floor=${toString cfg.balloon.floorGiB}"
                "--balloon-headroom=${toString cfg.balloon.headroomGiB}"
                "--balloon-slot=${if cfg.balloon.canarySlot == null then "" else cfg.balloon.canarySlot}"
                "--max-load-per-cpu=${toString cfg.maxLoadPerCpu}"
              ]
            );
            Restart = "always";
            RestartSec = "10s";
            RuntimeDirectory = "microvm-scaleset";
            RuntimeDirectoryMode = "0750";
            RuntimeDirectoryPreserve = "restart";
          };
        };

        microvm-nix-daemon = {
          wantedBy = [ "multi-user.target" ];
          serviceConfig = {
            ExecStart = "${lib.getExe pkgs.socat} VSOCK-LISTEN:${toString nixDaemonPort},fork,reuseaddr UNIX-CONNECT:/nix/var/nix/daemon-socket/socket";
            User = "microvm-nix";
            Group = "microvm-nix";
            Restart = "always";
          };
        };
      }
      // lib.genAttrs (map (n: "microvm@${n}") slotNames) (_: {
        # A VM powers off after its one job and waits for microvm-scaleset.
        serviceConfig = {
          Restart = lib.mkForce "no";
          CPUAccounting = true;
          MemoryAccounting = true;
          CPUWeight = cfg.vm.cpuWeight;
          CPUQuota = lib.mkIf (cfg.vm.cpuQuotaPercent != null) "${toString cfg.vm.cpuQuotaPercent}%";
        };
      });

      tmpfiles.rules = map (n: "d /run/microvm-jit/${n} 0750 root kvm -") slotNames;

      network = {
        enable = true;
        wait-online.enable = false;
        # Leave routes of other network managers alone; networkd only runs the
        # VM bridge.
        config.networkConfig = {
          ManageForeignRoutes = false;
          ManageForeignRoutingPolicyRules = false;
        };
        netdevs."20-${cfg.network.bridge}".netdevConfig = {
          Kind = "bridge";
          Name = cfg.network.bridge;
        };
        networks = {
          "20-${cfg.network.bridge}" = {
            matchConfig.Name = cfg.network.bridge;
            address = [ "${hostAddress}/24" ];
            networkConfig.ConfigureWithoutCarrier = true;
            linkConfig.RequiredForOnline = "no";
          };
          "21-microvm-github-runner-taps" = {
            matchConfig.Name = "ghrunner*";
            networkConfig.Bridge = cfg.network.bridge;
            linkConfig.RequiredForOnline = "no";
          };
        };
      };
    };

    networking = {
      networkmanager.unmanaged = lib.mkIf config.networking.networkmanager.enable [
        "interface-name:${cfg.network.bridge}"
        "interface-name:ghrunner*"
      ];
      nat = {
        enable = true;
        internalInterfaces = [ cfg.network.bridge ];
        inherit (cfg.network) externalInterface;
      };
      # VMs reach the internet, nothing on the host or private networks.
      firewall = {
        extraCommands = ''
          iptables -N microvm-in 2>/dev/null || iptables -F microvm-in
          iptables -A microvm-in -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
          iptables -A microvm-in -j DROP
          iptables -D INPUT -i ${cfg.network.bridge} -j microvm-in 2>/dev/null || true
          iptables -I INPUT -i ${cfg.network.bridge} -j microvm-in

          iptables -N microvm-fwd 2>/dev/null || iptables -F microvm-fwd
          for net in 10.0.0.0/8 100.64.0.0/10 169.254.0.0/16 172.16.0.0/12 192.168.0.0/16; do
            iptables -A microvm-fwd -d "$net" -j DROP
          done
          iptables -D FORWARD -i ${cfg.network.bridge} -j microvm-fwd 2>/dev/null || true
          iptables -I FORWARD -i ${cfg.network.bridge} -j microvm-fwd
        '';
        extraStopCommands = ''
          iptables -D INPUT -i ${cfg.network.bridge} -j microvm-in 2>/dev/null || true
          iptables -D FORWARD -i ${cfg.network.bridge} -j microvm-fwd 2>/dev/null || true
        '';
      };
    };
  };
}
