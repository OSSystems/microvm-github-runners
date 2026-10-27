# A runner VM: runs one job with the just-in-time config the host handed it
# for this boot, then powers off.
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.microvm-github-runner;
in
{
  options.microvm-github-runner =
    lib.genAttrs
      [
        "name"
        "address"
        "hostAddress"
        "mac"
        "tap"
      ]
      (
        _:
        lib.mkOption {
          type = lib.types.str;
          internal = true;
        }
      )
    //
      lib.genAttrs
        [
          "cid"
          "mem"
          "vcpu"
          "diskGiB"
          "nixDaemonPort"
        ]
        (
          _:
          lib.mkOption {
            type = lib.types.int;
            internal = true;
          }
        )
    // {
      initialMem = lib.mkOption {
        type = lib.types.ints.positive;
        default = cfg.mem;
        defaultText = lib.literalExpression "config.microvm-github-runner.mem";
        internal = true;
      };
      package = lib.mkOption {
        type = lib.types.package;
        default = pkgs.github-runner;
        defaultText = lib.literalExpression "pkgs.github-runner";
        description = "Runner package; its `bin/Runner.Listener` runs the job.";
      };
      packages = lib.mkOption {
        type = lib.types.listOf lib.types.package;
        default = [ ];
        description = "Extra packages on the job's PATH.";
      };
      environment = lib.mkOption {
        type = lib.types.attrsOf lib.types.str;
        default = { };
        description = "Extra environment for the runner.";
      };
    };

  config = {
    system.stateVersion = lib.mkDefault lib.trivial.release;
    # systemd picks up the JIT config from fw_cfg before the root is mounted.
    boot.initrd.kernelModules = [ "qemu_fw_cfg" ];
    boot.kernelModules = [ "virtio_balloon" ];
    networking = {
      hostName = cfg.name;
      useDHCP = false;
    };

    microvm = {
      hypervisor = "qemu";
      inherit (cfg) vcpu mem;
      # Free page reporting: memory a job frees goes back to the host instead
      # of staying with the VM until it powers off.
      balloon = true;
      vsock.cid = cfg.cid;
      interfaces = [
        {
          type = "tap";
          id = cfg.tap;
          inherit (cfg) mac;
        }
      ];
      shares = [
        {
          proto = "virtiofs";
          tag = "ro-store";
          source = "/nix/store";
          mountPoint = "/nix/.ro-store";
          readOnly = true;
        }
      ]
      ++ lib.optional (cfg.initialMem < cfg.mem) {
        proto = "virtiofs";
        tag = "runner-control";
        source = "/run/microvm-jit/${cfg.name}/control";
        mountPoint = "/run/runner-control";
        readOnly = true;
      };
      credentialFiles.jitconfig = "/run/microvm-jit/${cfg.name}/jitconfig";
      # Docker data, the runner workspace and home all live here, and the image
      # is deleted before every boot.
      volumes = [
        {
          image = "var.img";
          mountPoint = "/var";
          # The guest caches the disk already; the host caching it too would
          # count against the VM's memory.
          direct = true;
          size = cfg.diskGiB * 1024;
        }
      ];
      preStart = "rm -f var.img";
    };

    systemd.network = {
      enable = true;
      networks."10-uplink" = {
        matchConfig.MACAddress = cfg.mac;
        address = [ "${cfg.address}/24" ];
        gateway = [ cfg.hostAddress ];
        dns = [
          "1.1.1.1"
          "8.8.8.8"
        ];
      };
    };

    # Nix talks to the host's daemon over vsock; the store itself is the
    # host's, read-only. The settings live in nix.conf rather than NIX_CONFIG,
    # which jobs set for themselves and would replace them.
    nix.enable = false;
    environment.variables.NIX_REMOTE = "daemon";
    environment.etc."nix/nix.conf".text = ''
      experimental-features = nix-command flakes
    '';
    systemd.sockets.nix-daemon-proxy = {
      wantedBy = [ "sockets.target" ];
      socketConfig = {
        ListenStream = "/nix/var/nix/daemon-socket/socket";
        Accept = true;
      };
    };
    systemd.services."nix-daemon-proxy@".serviceConfig = {
      ExecStart = "${lib.getExe pkgs.socat} STDIO VSOCK-CONNECT:2:${toString cfg.nixDaemonPort}";
      StandardInput = "socket";
    };

    # The VM is the boundary, so jobs get a normal rootful Docker.
    virtualisation.docker.enable = true;
    systemd = {
      # Compose driven as a Go library (testcontainers) only finds the CLI
      # plugins in the standard directories, not through the docker wrapper.
      tmpfiles.rules = [
        "L+ /usr/libexec/docker/cli-plugins/docker-buildx - - - - ${pkgs.docker-buildx}/libexec/docker/cli-plugins/docker-buildx"
        "L+ /usr/libexec/docker/cli-plugins/docker-compose - - - - ${pkgs.docker-compose}/libexec/docker/cli-plugins/docker-compose"
      ];
      # tonistiigi/binfmt registers QEMU from a privileged container, which only
      # outlives the container when binfmt_misc is mounted.
      mounts = [
        {
          what = "binfmt_misc";
          where = "/proc/sys/fs/binfmt_misc";
          type = "binfmt_misc";
          wantedBy = [ "sysinit.target" ];
        }
      ];
    };

    users.users.runner = {
      isNormalUser = true;
      home = "/var/lib/runner";
      extraGroups = [
        "docker"
        "wheel"
      ];
    };
    security.sudo.wheelNeedsPassword = false;

    systemd.services.runner-memory-ready = lib.mkIf (cfg.initialMem < cfg.mem) {
      unitConfig = {
        RequiresMountsFor = "/run/runner-control";
        FailureAction = "poweroff";
      };
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        TimeoutStartSec = "125s";
      };
      script = ''
        for ((attempt = 0; attempt < 120; attempt++)); do
          if test -f /run/runner-control/ready; then
            exit 0
          fi
          ${pkgs.coreutils}/bin/sleep 1
        done
        echo "Host did not establish the initial memory allocation" >&2
        exit 1
      '';
    };

    systemd.services.github-runner = {
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      requires = lib.optional (cfg.initialMem < cfg.mem) "runner-memory-ready.service";
      after = [
        "network-online.target"
        "docker.service"
      ]
      ++ lib.optional (cfg.initialMem < cfg.mem) "runner-memory-ready.service";
      # The setuid sudo; the one in the store is not.
      path = [
        "/run/wrappers"
      ]
      ++ (with pkgs; [
        bash
        config.nix.package
        config.virtualisation.docker.package
        coreutils
        findutils
        git
        gnugrep
        gnused
        gnutar
        gzip
      ])
      ++ cfg.packages;
      environment = {
        RUNNER_ROOT = "/var/lib/github-runner";
        inherit (config.environment.variables) NIX_REMOTE;
      }
      // cfg.environment;
      unitConfig = {
        SuccessAction = "poweroff";
        FailureAction = "poweroff";
      };
      serviceConfig = {
        User = "runner";
        StateDirectory = "github-runner";
        WorkingDirectory = "/var/lib/github-runner";
        ImportCredential = "jitconfig";
        ExecStart = pkgs.writeShellScript "github-runner-jit" ''
          exec ${cfg.package}/bin/Runner.Listener run \
            --jitconfig "$(<"$CREDENTIALS_DIRECTORY/jitconfig")"
        '';
        KillSignal = "SIGINT";
      };
    };
  };
}
