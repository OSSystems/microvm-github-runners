# A runner VM on cloud-hypervisor.
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.microvm-github-runner;

  controlRequest = timeoutSec: {
    inherit timeoutSec;
    args = [
      "-t"
      (toString timeoutSec)
      "-"
      "VSOCK-CONNECT:2:${toString cfg.controlPort}"
    ];
  };
  requests = cfg.controlRequests;
  vsockRequest = request: "${lib.getExe pkgs.socat} ${lib.escapeShellArgs request.args}";

  movableRatio = (100 * (cfg.maxMemMiB - cfg.bootMemMiB) + cfg.bootMemMiB - 1) / cfg.bootMemMiB;
in
{
  imports = [ ./guest-common.nix ];

  options.microvm-github-runner =
    lib.genAttrs
      [
        "controlPort"
        "bootMemMiB"
        "maxMemMiB"
      ]
      (
        _:
        lib.mkOption {
          type = lib.types.int;
          internal = true;
        }
      )
    // {
      controlRequests = lib.mkOption {
        type = lib.types.raw;
        readOnly = true;
        internal = true;
        default = {
          jitconfig = controlRequest 120 // {
            attempts = 5;
          };
          stats = controlRequest 1;
        };
        description = "How the guest talks to the controller's control channel.";
      };
    };

  config = {
    microvm = {
      hypervisor = "cloud-hypervisor";
      balloon = true;
    };

    boot.kernelParams = [
      "memhp_default_state=online"
      "memory_hotplug.online_policy=auto-movable"
      "memory_hotplug.auto_movable_ratio=${toString movableRatio}"
      "memory_hotplug.memmap_on_memory=1"
      "psi=1"
    ];
    boot.kernelModules = [ "virtio_mem" ];
    services.udev.extraRules = ''
      SUBSYSTEM=="cpu", ATTR{online}=="0", ATTR{online}="1"
    '';

    fileSystems."/var" = {
      device = "/dev/vda";
      fsType = "ext4";
      autoFormat = true;
      noCheck = true;
    };
    fileSystems."/tmp" = {
      device = "/var/.tmp";
      fsType = "none";
      options = [
        "bind"
        "x-systemd.requires-mounts-for=/var"
      ];
    };
    systemd.services.runner-tmp = {
      requiredBy = [ "tmp.mount" ];
      before = [ "tmp.mount" ];
      unitConfig = {
        DefaultDependencies = false;
        RequiresMountsFor = "/var";
      };
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = "${pkgs.coreutils}/bin/install -d -m 1777 /var/.tmp";
      };
    };

    networking.hostName = "";
    system.name = "microvm-github-runner";
    systemd.services.runner-identity = {
      wantedBy = [ "sysinit.target" ];
      before = [
        "sysinit.target"
        "systemd-networkd.service"
        "network-pre.target"
      ];
      wants = [ "network-pre.target" ];
      unitConfig = {
        DefaultDependencies = false;
        FailureAction = "poweroff";
      };
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
      };
      script = ''
        for arg in $(</proc/cmdline); do
          case "$arg" in
            ghr.name=*) name=''${arg#ghr.name=} ;;
            ghr.addr=*) addr=''${arg#ghr.addr=} ;;
            ghr.gw=*) gw=''${arg#ghr.gw=} ;;
          esac
        done
        if [ -z "''${name:-}" ] || [ -z "''${addr:-}" ] || [ -z "''${gw:-}" ]; then
          echo "ghr.name, ghr.addr or ghr.gw missing from the kernel command line" >&2
          exit 1
        fi
        echo "$name" >/proc/sys/kernel/hostname
        mkdir -p /run/systemd/network
        cat >/run/systemd/network/10-uplink.network <<NETWORK
        [Match]
        Driver=virtio_net

        [Network]
        Address=$addr
        Gateway=$gw
        DNS=1.1.1.1
        DNS=8.8.8.8
        NETWORK
      '';
    };

    systemd.services.runner-jitconfig = {
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      unitConfig.FailureAction = "poweroff";
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        RuntimeDirectory = "runner-jitconfig";
        RuntimeDirectoryMode = "0700";
        TimeoutStartSec = requests.jitconfig.attempts * (requests.jitconfig.timeoutSec + 1) + 30;
      };
      path = [ pkgs.jq ];
      script = ''
        for _ in $(seq ${toString requests.jitconfig.attempts}); do
          reply=$(echo '{"op":"jitconfig"}' | ${vsockRequest requests.jitconfig}) && break
          sleep 1
        done
        if ! jitconfig=$(jq -er '.jitconfig' <<<"''${reply:-}"); then
          echo "No just-in-time config from the host: ''${reply:-}" >&2
          exit 1
        fi
        (umask 077; printf '%s' "$jitconfig" >/run/runner-jitconfig/jitconfig)
      '';
    };

    systemd.services.runner-stats = {
      wantedBy = [ "multi-user.target" ];
      after = [ "runner-jitconfig.service" ];
      serviceConfig.Restart = "always";
      path = [ pkgs.gawk ];
      script = ''
        while true; do
          stats=$(awk '
            FILENAME == "/proc/meminfo" && $1 == "MemAvailable:" { avail = $2 }
            FILENAME == "/proc/meminfo" && $1 == "MemTotal:" { total = $2 }
            FILENAME == "/proc/pressure/memory" { sub("avg10=", "", $2); psi[$1] = $2 }
            END {
              printf "{\"op\":\"stats\",\"memAvailableKiB\":%d,\"memTotalKiB\":%d,\"psiSomeAvg10\":%s,\"psiFullAvg10\":%s}\n",
                avail, total, psi["some"] + 0, psi["full"] + 0
            }' /proc/meminfo /proc/pressure/memory)
          echo "$stats" | ${vsockRequest requests.stats} >/dev/null || true
          sleep 2
        done
      '';
    };

    systemd.services.github-runner = {
      requires = [ "runner-jitconfig.service" ];
      after = [ "runner-jitconfig.service" ];
      serviceConfig.LoadCredential = "jitconfig:/run/runner-jitconfig/jitconfig";
    };
  };
}
