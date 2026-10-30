{
  description = "GitHub Actions runners in throwaway microVMs, scaled to the host's load";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
    microvm = {
      url = "github:microvm-nix/microvm.nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs =
    {
      self,
      nixpkgs,
      microvm,
      ...
    }:
    let
      forAllSystems = nixpkgs.lib.genAttrs [
        "x86_64-linux"
        "aarch64-linux"
      ];
    in
    {
      packages = forAllSystems (system: rec {
        microvm-scaleset = nixpkgs.legacyPackages.${system}.callPackage ./package.nix { };
        default = microvm-scaleset;
      });

      nixosModules.default = {
        imports = [
          microvm.nixosModules.host
          ./modules/host.nix
        ];
        services.microvm-github-runners.microvmGuestModule = microvm.nixosModules.microvm;
      };

      checks = forAllSystems (
        system:
        let
          inherit (nixpkgs) lib;
          hostConfig =
            backend: extra:
            (nixpkgs.lib.nixosSystem {
              inherit system;
              modules = [
                self.nixosModules.default
                {
                  boot.loader.grub.enable = false;
                  fileSystems."/" = {
                    device = "/dev/vda";
                    fsType = "ext4";
                  };
                  system.stateVersion = "26.05";
                  networking = {
                    hostName = "ci-host";
                    useDHCP = false;
                  };
                  services.microvm-github-runners = {
                    enable = true;
                    inherit backend;
                    urls = [ "https://github.com/my-org" ];
                    githubApp = {
                      id = "1";
                      privateKeyFile = "/run/keys/github-app.pem";
                    };
                    network.externalInterface = "eth0";
                  }
                  // extra;
                }
              ];
            }).config;
          host = backend: extra: (hostConfig backend extra).system.build.toplevel;
          evaluated =
            name: toplevel:
            nixpkgs.legacyPackages.${system}.writeText "${name}-eval" (
              builtins.unsafeDiscardStringContext toplevel.drvPath
            );
        in
        {
          host-cloud-hypervisor = evaluated "host-cloud-hypervisor" (host "cloud-hypervisor" { });
          host-qemu = evaluated "host-qemu" (host "qemu" { poolSize = 2; });
          controller-flags =
            let
              execStart =
                backend: extra:
                (hostConfig backend extra).systemd.services.microvm-scaleset.serviceConfig.ExecStart;
            in
            nixpkgs.legacyPackages.${system}.runCommand "controller-flags" { } ''
              ${execStart "cloud-hypervisor" { }} --help
              ${execStart "qemu" { poolSize = 2; }} --help
              touch $out
            '';
          guest-cloud-hypervisor =
            let
              ch = hostConfig "cloud-hypervisor" { };
              qemu = hostConfig "qemu" { poolSize = 2; };
              guest = ch.services.microvm-github-runners.guestConfig;
              cfg = guest.microvm-github-runner;
              params = guest.microvm.kernelParams;
              param = name: lib.findFirst (p: lib.hasPrefix "${name}=" p) null params;
              movableRatio = param "memory_hotplug.auto_movable_ratio";
              services = guest.systemd.services;
              requests = cfg.controlRequests;
              boundedOnlyAfterSending =
                request:
                !lib.any (lib.hasPrefix "-T") request.args
                && lib.any (
                  i:
                  lib.elemAt request.args i == "-t" && lib.elemAt request.args (i + 1) == toString request.timeoutSec
                ) (lib.range 0 (lib.length request.args - 2));
              tmp = guest.fileSystems."/tmp" or null;
              failures = lib.filter (c: !c.ok) [
                {
                  ok =
                    movableRatio != null
                    &&
                      lib.toInt (lib.removePrefix "memory_hotplug.auto_movable_ratio=" movableRatio) * cfg.bootMemMiB
                      >= 100 * (cfg.maxMemMiB - cfg.bootMemMiB);
                  msg = "auto_movable_ratio must let ZONE_MOVABLE take all hotpluggable memory, got ${toString movableRatio}";
                }
                {
                  ok = param "memory_hotplug.memmap_on_memory" == "memory_hotplug.memmap_on_memory=1";
                  msg = "hotplugged memory must carry its own memmap";
                }
                {
                  ok = boundedOnlyAfterSending requests.jitconfig && boundedOnlyAfterSending requests.stats;
                  msg = "control requests must be bounded by -t alone";
                }
                {
                  ok =
                    services.runner-jitconfig.serviceConfig.TimeoutStartSec or 0
                    >= requests.jitconfig.timeoutSec * requests.jitconfig.attempts;
                  msg = "runner-jitconfig must outlast every attempt of its request";
                }
                {
                  ok =
                    lib.toList (ch.systemd.services.microvm-scaleset.serviceConfig.ExecStartPre or [ ]) == [
                      "${
                        nixpkgs.legacyPackages.${system}.coreutils
                      }/bin/ln -sfn ${ch.services.microvm-github-runners.guestManifest} /nix/var/nix/gcroots/microvm-scaleset-guest"
                    ];
                  msg = "the guest manifest the controller starts with must be a GC root";
                }
                {
                  ok = ch.systemd.services.microvm-scaleset.restartIfChanged == false;
                  msg = "a deploy must not restart microvm-scaleset";
                }
                {
                  ok =
                    tmp != null
                    && lib.hasPrefix "/var/" tmp.device
                    && lib.elem "bind" tmp.options
                    && lib.elem "tmp.mount" (services.runner-tmp.requiredBy or [ ])
                    && lib.elem "tmp.mount" (services.runner-tmp.before or [ ]);
                  msg = "/tmp must be a bind of a directory on the VM disk, created before it is mounted";
                }
                {
                  ok = lib.any (o: builtins.match "size=[0-9]+%" o != null) (
                    lib.concatMap (lib.splitString ",") guest.fileSystems."/".options
                  );
                  msg = "the root tmpfs must stay bounded by a share of the boot memory";
                }
                {
                  ok = (services.runner-identity.unitConfig.FailureAction or null) == "poweroff";
                  msg = "a VM without identity must power off";
                }
                {
                  ok =
                    !(ch.systemd.network.networks ? "21-microvm-github-runner-taps")
                    && qemu.systemd.network.networks ? "21-microvm-github-runner-taps";
                  msg = "only the QEMU backend leaves tap bridging to networkd";
                }
              ];
            in
            if failures == [ ] then
              nixpkgs.legacyPackages.${system}.writeText "guest-cloud-hypervisor-eval" "ok"
            else
              throw ("guest-cloud-hypervisor: " + lib.concatMapStringsSep "; " (c: c.msg) failures);
        }
      );

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.nixfmt);
    };
}
