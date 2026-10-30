# What every runner VM shares, whatever the hypervisor.
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
  options.microvm-github-runner = {
    nixDaemonPort = lib.mkOption {
      type = lib.types.int;
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
    boot.kernelModules = [ "virtio_balloon" ];
    networking.useDHCP = false;

    microvm.shares = [
      {
        proto = "virtiofs";
        tag = "ro-store";
        source = "/nix/store";
        mountPoint = "/nix/.ro-store";
        readOnly = true;
      }
    ];

    systemd.network.enable = true;

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

    systemd.services.github-runner = {
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [
        "network-online.target"
        "docker.service"
      ];
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
        ExecStart = pkgs.writeShellScript "github-runner-jit" ''
          exec ${cfg.package}/bin/Runner.Listener run \
            --jitconfig "$(<"$CREDENTIALS_DIRECTORY/jitconfig")"
        '';
        KillSignal = "SIGINT";
      };
    };
  };
}
