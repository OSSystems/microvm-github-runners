# A runner VM on the legacy QEMU backend: runs one job with the just-in-time
# config the host handed it for this boot, then powers off.
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
  imports = [ ./guest-common.nix ];

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
    };

  config = {
    # systemd picks up the JIT config from fw_cfg before the root is mounted.
    boot.initrd.kernelModules = [ "qemu_fw_cfg" ];
    networking.hostName = cfg.name;

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
      shares = lib.optional (cfg.initialMem < cfg.mem) {
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

    systemd.network.networks."10-uplink" = {
      matchConfig.MACAddress = cfg.mac;
      address = [ "${cfg.address}/24" ];
      gateway = [ cfg.hostAddress ];
      dns = [
        "1.1.1.1"
        "8.8.8.8"
      ];
    };

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
      requires = lib.optional (cfg.initialMem < cfg.mem) "runner-memory-ready.service";
      after = lib.mkAfter (lib.optional (cfg.initialMem < cfg.mem) "runner-memory-ready.service");
      serviceConfig.ImportCredential = "jitconfig";
    };
  };
}
