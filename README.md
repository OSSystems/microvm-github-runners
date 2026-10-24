# microvm-github-runners

GitHub Actions self-hosted runners for NixOS where **every job runs in its own
throwaway microVM**, and the number of VMs follows the host's spare CPU and
memory.

Built on [microvm.nix](https://github.com/microvm-nix/microvm.nix) and the
[actions/scaleset](https://github.com/actions/scaleset) client. No Kubernetes.

## Why

Self-hosted runners usually share one long-lived machine. Anything a job
leaves behind (Docker images, caches, tokens in `$HOME`, a root-equivalent
Docker socket) is there for the next job, including jobs from pull requests.
Here each job gets:

- a fresh VM with a fresh disk, destroyed when the job ends;
- its own rootful Docker daemon (with buildx, compose and QEMU binfmt), since
  the VM is the boundary;
- a single-use just-in-time runner config. The GitHub App key stays on the
  host and never enters a VM;
- Nix through the host's daemon as an untrusted user: builds and their cache
  land in the host store, while the VM sees `/nix/store` read-only;
- a network that reaches the internet but not the host or private ranges.

## How it scales

`microvm-scaleset` registers a [runner scale set](https://github.com/actions/scaleset#what-is-a-scale-set)
and tells GitHub how many jobs the host can take right now. It boots one VM
from a pool per assigned job, and grows only while:

- the 1-minute load average per CPU is below `maxLoadPerCpu`, and
- available memory still covers another VM after `memReserveGiB` and the
  memory already-running VMs may still grow into.

When the host is busy it stops accepting jobs; they stay queued on GitHub
instead of overloading the machine.

## Usage

```nix
{
  inputs.microvm-github-runners.url = "github:OSSystems/microvm-github-runners";

  # in your NixOS configuration
  imports = [ inputs.microvm-github-runners.nixosModules.default ];

  services.microvm-github-runners = {
    enable = true;
    url = "https://github.com/my-org";
    name = "my-microvm";            # workflows use `runs-on: my-microvm`
    githubApp = {
      id = "123456";
      privateKeyFile = "/run/secrets/github-app-key";
    };
    network.externalInterface = "enp6s0";
    poolSize = 8;
    vm = { memGiB = 10; vcpu = 8; diskGiB = 100; };

    # Anything jobs need on top of the base image.
    guestModules = [
      ({ pkgs, ... }: {
        microvm-github-runner.packages = with pkgs; [ gcc gnumake jq ];
      })
    ];
  };
}
```

The GitHub App needs organization *Self-hosted runners: read & write*
permission.

The host needs KVM. VMs use QEMU, a `/24` bridge with NAT (`network.subnet`,
default `10.231.0`) and vsock CIDs 3 and up.

## Status

Early, but running real CI. The scale set client is a public preview, so its
API may still change.
