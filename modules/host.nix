# Runs GitHub Actions jobs in throwaway microVMs. microvm-scaleset boots VMs
# shared by every organization while the host has room, each with a fresh disk and a
# just-in-time runner config, so the GitHub App key never enters a VM and
# nothing outlives the job.
{
  config,
  options,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.microvm-github-runners;
  hostAddress = "${cfg.network.subnet}.1";
  nixDaemonPort = 5000;
  controlPort = 5001;
  package = pkgs.callPackage ../package.nix { };
  isQemu = cfg.backend == "qemu";
  commonFlags = map (url: "--url=${url}") cfg.urls ++ [
    "--name=${cfg.name}"
    "--labels=${lib.concatStringsSep "," cfg.labels}"
    "--app-id=${cfg.githubApp.id}"
    "--private-key-file=${cfg.githubApp.privateKeyFile}"
    "--max-load-per-cpu=${toString cfg.maxLoadPerCpu}"
  ];

  slots = builtins.genList (i: {
    name = "${cfg.name}-${toString (i + 1)}";
    index = i + 1;
  }) cfg.poolSize;
  slotNames = map (s: s.name) slots;

  bootMemMiB = 2048;
  hotplugMemMiB = 1048576;
  guestSystem = import (pkgs.path + "/nixos/lib/eval-config.nix") {
    inherit pkgs;
    inherit (pkgs.stdenv.hostPlatform) system;
    specialArgs = cfg.guestSpecialArgs;
    prefix = [
      "services"
      "microvm-github-runners"
      "guest"
    ];
    modules = [
      cfg.microvmGuestModule
      ./guest-ch.nix
      {
        microvm-github-runner = {
          inherit nixDaemonPort controlPort bootMemMiB;
          maxMemMiB = bootMemMiB + hotplugMemMiB;
        };
      }
    ]
    ++ cfg.guestModules;
  };
  guest = guestSystem.config;
  guestBoot =
    system: guest:
    let
      linuxTarget = pkgs.linux.target or pkgs.stdenv.hostPlatform.linux-kernel.target;
      perSystem =
        {
          x86_64-linux = {
            kernel = "${guest.microvm.kernel.dev}/vmlinux";
            console = [
              "earlyprintk=ttyS0"
              "console=ttyS0"
            ];
          };
          aarch64-linux = {
            kernel = "${guest.microvm.kernel.out}/${linuxTarget}";
            console = [ "console=ttyAMA0" ];
          };
        }
        .${system}
          or (throw "microvm-github-runners: cloud-hypervisor guests are unsupported on ${system}");
    in
    {
      inherit (perSystem) kernel;
      cmdline = lib.concatStringsSep " " (
        perSystem.console
        ++ [
          "reboot=t"
          "panic=-1"
        ]
        ++ guest.microvm.kernelParams
      );
    };
  guestManifest = pkgs.writeText "microvm-github-runner-guest.json" (
    builtins.toJSON {
      cloudHypervisor = lib.getExe pkgs.cloud-hypervisor;
      virtiofsd = lib.getExe pkgs.virtiofsd;
      inherit (guestBoot pkgs.stdenv.hostPlatform.system guest) kernel cmdline;
      initrd = guest.microvm.initrdPath;
      shares = map (share: { inherit (share) tag source; }) (
        lib.filter (share: share.proto == "virtiofs") guest.microvm.shares
      );
      inherit bootMemMiB hotplugMemMiB;
    }
  );

  qemuOnlyOptions = [
    [ "poolSize" ]
    [
      "vm"
      "memGiB"
    ]
    [
      "vm"
      "initialMemGiB"
    ]
    [
      "vm"
      "vcpu"
    ]
    [
      "vm"
      "cpuWeight"
    ]
    [
      "vm"
      "cpuQuotaPercent"
    ]
    [
      "vm"
      "diskGiB"
    ]
    [ "memReserveGiB" ]
    [ "memCommitRatio" ]
    [
      "balloon"
      "enable"
    ]
    [
      "balloon"
      "floorGiB"
    ]
    [
      "balloon"
      "headroomGiB"
    ]
    [
      "balloon"
      "canarySlot"
    ]
  ];
  qemuOnlySet = lib.filter (
    path:
    (lib.getAttrFromPath path options.services.microvm-github-runners).highestPrio
    < (lib.mkOptionDefault null).priority
  ) qemuOnlyOptions;
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

    backend = lib.mkOption {
      type = lib.types.enum [
        "cloud-hypervisor"
        "qemu"
      ];
      default = "cloud-hypervisor";
      description = ''
        Hypervisor the VMs run on. With `cloud-hypervisor` nothing is sized
        per host: every VM may use all host CPUs and grows or shrinks its
        memory as the job needs, and new VMs boot while the host has CPU,
        memory and disk to spare. `qemu` is the legacy fixed pool of
        `poolSize` VMs sized by `vm.*`; the options marked QEMU-only apply to
        it alone.
      '';
    };

    urls = lib.mkOption {
      type = lib.types.nonEmptyListOf lib.types.str;
      example = [
        "https://github.com/my-org"
        "https://github.com/my-other-org"
      ];
      description = ''
        Organizations to register a scale set with. They all share the
        host, so jobs of a busy organization use the room idle ones leave.
      '';
    };
    name = lib.mkOption {
      type = lib.types.str;
      default = config.networking.hostName;
      defaultText = lib.literalExpression "config.networking.hostName";
      description = ''
        Scale set name in every organization; also prefixes the VM names.
        Scale set names are unique per organization, so each host keeps its
        own.
      '';
    };
    labels = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ cfg.name ];
      defaultText = lib.literalExpression "[ name ]";
      example = lib.literalExpression ''[ config.networking.hostName "nix-vm" ]'';
      description = ''
        Labels workflows use in `runs-on`. For a fleet, give every host one
        common label and target that, so a job runs on whichever host takes
        it first.
      '';
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
      description = "QEMU only. Most VMs that may run at once, across all organizations; how many do depends on load.";
    };
    vm = {
      memGiB = lib.mkOption {
        type = lib.types.ints.positive;
        default = 10;
      };
      initialMemGiB = lib.mkOption {
        type = lib.types.ints.positive;
        default = cfg.vm.memGiB;
        defaultText = lib.literalExpression "config.services.microvm-github-runners.vm.memGiB";
        description = "Guest allocation established before the runner starts; memGiB remains the maximum.";
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
    memReservePercent = lib.mkOption {
      type = lib.types.ints.between 0 99;
      default = 10;
      description = ''
        cloud-hypervisor only. Share of host memory, at least 2 GiB, left to
        the host: no VM boots and no VM grows into it.
      '';
    };
    diskReservePercent = lib.mkOption {
      type = lib.types.ints.between 0 99;
      default = 10;
      description = "cloud-hypervisor only. Share of the `workDir` file system no new VM boots into.";
    };
    workDir = lib.mkOption {
      type = lib.types.str;
      default = "/var/lib/microvm-scaleset";
      description = ''
        cloud-hypervisor only. Where each VM's disk and sockets live while it
        runs. Disks are sparse and grow with what the job writes.
      '';
    };
    memReserveGiB = lib.mkOption {
      type = lib.types.ints.unsigned;
      default = 8;
      description = "QEMU only. Memory always left for the host when deciding to boot another VM.";
    };
    memCommitRatio = lib.mkOption {
      type = lib.types.float;
      default = 1.0;
      example = 1.3;
      description = ''
        QEMU only. Maximum committed allocations divided by host RAM after memReserveGiB.
        1.0 permits 100%, 1.3 permits 130%, and 2.0 permits 200%.
        Available host memory and pending VM growth also limit new boots.
        Elastic guests commit their current allocation, not their maximum ceiling.
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

    microvmGuestModule = lib.mkOption {
      type = lib.types.nullOr lib.types.raw;
      default = null;
      internal = true;
      description = "microvm.nix's guest module (`nixosModules.microvm`); `nixosModules.default` sets it.";
    };
    guestConfig = lib.mkOption {
      type = lib.types.raw;
      readOnly = true;
      internal = true;
      default = guest;
      defaultText = lib.literalMD "the evaluated cloud-hypervisor guest configuration";
      description = "The guest system's configuration, for checks.";
    };
    guestManifest = lib.mkOption {
      type = lib.types.path;
      readOnly = true;
      internal = true;
      default = guestManifest;
      defaultText = lib.literalMD "the cloud-hypervisor guest manifest";
      description = "Kernel, initrd, command line and shares microvm-scaleset boots cloud-hypervisor VMs from.";
    };
  };

  config = lib.mkIf cfg.enable (
    lib.mkMerge [
      (lib.mkIf isQemu {
        assertions = [
          {
            assertion = cfg.memCommitRatio >= 1.0;
            message = "microvm-github-runners.memCommitRatio must be at least 1.0.";
          }
          {
            assertion = cfg.balloon.floorGiB <= cfg.vm.initialMemGiB && cfg.vm.initialMemGiB <= cfg.vm.memGiB;
            message = "microvm-github-runners requires balloon.floorGiB <= vm.initialMemGiB <= vm.memGiB.";
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
                imports = [ ./guest-qemu.nix ] ++ cfg.guestModules;
                microvm-github-runner = {
                  inherit (slot) name;
                  inherit hostAddress nixDaemonPort;
                  inherit (cfg.vm) vcpu diskGiB;
                  address = "${cfg.network.subnet}.${toString (10 + slot.index)}";
                  mac = "02:00:0a:e7:00:${lib.fixedWidthString 2 "0" (lib.toHexString slot.index)}";
                  tap = "ghrunner${toString slot.index}";
                  cid = 2 + slot.index;
                  mem = cfg.vm.memGiB * 1024;
                  initialMem = cfg.vm.initialMemGiB * 1024;
                };
              };
            };
          }) slots
        );

        systemd = {
          services = {
            microvm-scaleset.serviceConfig.ExecStart = lib.escapeShellArgs (
              [
                (lib.getExe package)
                "--backend=qemu"
              ]
              ++ commonFlags
              ++ [
                "--slots=${lib.concatStringsSep "," slotNames}"
                "--vm-mem=${toString cfg.vm.memGiB}"
                "--vm-initial-mem=${toString cfg.vm.initialMemGiB}"
                "--mem-reserve=${toString cfg.memReserveGiB}"
                "--mem-commit-ratio=${toString cfg.memCommitRatio}"
                "--state-dir=${config.microvm.stateDir}"
                "--balloon-enable=${lib.boolToString cfg.balloon.enable}"
                "--balloon-floor=${toString cfg.balloon.floorGiB}"
                "--balloon-headroom=${toString cfg.balloon.headroomGiB}"
                "--balloon-slot=${if cfg.balloon.canarySlot == null then "" else cfg.balloon.canarySlot}"
              ]
            );

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

          network.networks."21-microvm-github-runner-taps" = {
            matchConfig.Name = "ghrunner*";
            networkConfig.Bridge = cfg.network.bridge;
            linkConfig.RequiredForOnline = "no";
          };

          tmpfiles.rules = lib.concatMap (n: [
            "d /run/microvm-jit/${n} 0750 root kvm -"
            "d /run/microvm-jit/${n}/control 0755 root root -"
          ]) slotNames;
        };
      })

      (lib.mkIf (!isQemu) {
        assertions = [
          {
            assertion = qemuOnlySet == [ ];
            message = ''
              microvm-github-runners: the options ${
                lib.concatMapStringsSep ", " (lib.concatStringsSep ".") qemuOnlySet
              } apply only to backend = "qemu". With cloud-hypervisor nothing is sized per
              host; remove them, or set backend = "qemu" to keep the fixed pool.
            '';
          }
          {
            assertion = cfg.microvmGuestModule != null;
            message = "microvm-github-runners: import the flake's nixosModules.default, which provides microvm.nix's guest module.";
          }
        ];

        users.users.microvm = {
          isSystemUser = true;
          group = "kvm";
        };

        systemd = {
          services.microvm-scaleset = {
            restartIfChanged = false;
            serviceConfig.ExecStartPre = "${pkgs.coreutils}/bin/ln -sfn ${guestManifest} /nix/var/nix/gcroots/microvm-scaleset-guest";
            serviceConfig.ExecStart = lib.escapeShellArgs (
              [
                (lib.getExe package)
                "--backend=cloud-hypervisor"
              ]
              ++ commonFlags
              ++ [
                "--vm-user=microvm"
                "--guest-manifest=${guestManifest}"
                "--bridge=${cfg.network.bridge}"
                "--subnet=${cfg.network.subnet}"
                "--work-dir=${cfg.workDir}"
                "--mem-reserve-percent=${toString cfg.memReservePercent}"
                "--disk-reserve-percent=${toString cfg.diskReservePercent}"
                "--nix-daemon-port=${toString nixDaemonPort}"
                "--control-port=${toString controlPort}"
              ]
            );
          };
          tmpfiles.rules = [ "d ${cfg.workDir} 0750 root kvm -" ];
        };
      })

      {
        assertions = [
          {
            assertion = cfg.name != "";
            message = "microvm-github-runners.name is empty; set it or networking.hostName.";
          }
        ];

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
          services.microvm-scaleset = {
            wantedBy = [ "multi-user.target" ];
            wants = [ "network-online.target" ];
            after = [ "network-online.target" ];
            path = [ config.systemd.package ];
            serviceConfig = {
              Restart = "always";
              RestartSec = "10s";
              RuntimeDirectory = "microvm-scaleset";
              RuntimeDirectoryMode = "0750";
              RuntimeDirectoryPreserve = "restart";
            };
          };

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
      }
    ]
  );
}
