# microvm-github-runners

GitHub Actions self-hosted runners for NixOS where **every job runs in its own
throwaway microVM**, and the number of VMs follows the host's spare CPU and
memory.

Built on [cloud-hypervisor](https://www.cloudhypervisor.org/),
[microvm.nix](https://github.com/microvm-nix/microvm.nix) and the
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
in every organization listed in `urls` and tells GitHub how many jobs the host
can take right now. All organizations share the host: an idle one holds
nothing, and when several have jobs waiting, they take turns. Each assigned job
gets one [cloud-hypervisor](https://www.cloudhypervisor.org/) VM.

Nothing is sized per host. Every VM sees all host CPUs, boots with 2 GiB of
memory and grows from there: the guest reports its available memory and
memory pressure every two seconds, and the controller plugs memory in 1 GiB
steps (virtio-mem) while the guest is under pressure and unplugs it once the
guest has had plenty to spare for a while. Memory a job frees goes back to the
host through free page reporting. Under CPU contention VMs share the CPUs
equally.

A new VM boots only while:

- the 1-minute load average per CPU is below `maxLoadPerCpu`,
- available host memory, minus the reserve and VMs still booting, covers the
  boot memory, and
- the `workDir` file system has more free space than its reserve.

The memory reserve is `memReservePercent` (default 10%) of host RAM, at least
2 GiB; no VM grows into it either, so when the host is full a guest under
pressure may run out of memory, never the host. The disk reserve is
`diskReservePercent` (default 10%). Each VM's disk is an empty sparse file the
guest formats on boot, and only takes the space the job writes. `/tmp` lives
on that disk too, so large temporary files cost disk, not memory.

When the host is busy it boots no new VMs; the jobs it accepted wait for one
instead of overloading the machine.

## Usage

```nix
{
  inputs.microvm-github-runners.url = "github:OSSystems/microvm-github-runners";

  # in your NixOS configuration
  imports = [ inputs.microvm-github-runners.nixosModules.default ];

  services.microvm-github-runners = {
    enable = true;
    urls = [                        # all share the host
      "https://github.com/my-org"
      "https://github.com/my-other-org"
    ];
    githubApp = {
      id = "123456";
      privateKeyFile = "/run/secrets/github-app-key";
    };
    network.externalInterface = "enp6s0";

    # Anything jobs need on top of the base image.
    guestModules = [
      ({ pkgs, ... }: {
        microvm-github-runner.packages = with pkgs; [ gcc gnumake jq ];
      })
    ];
  };
}
```

The scale set is named after the host (`name` defaults to
`networking.hostName`) and workflows can target it with `runs-on: <hostname>`.
To run a fleet, import the module on every machine and give them all one
extra label, which workflows target instead:

```nix
services.microvm-github-runners.labels = [ config.networking.hostName "nix-vm" ];
```

```yaml
runs-on: nix-vm
```

Adding a machine is then importing the module there; its jobs come from the
same queue.

The GitHub App needs organization *Self-hosted runners: read & write*
permission and must be installed on every organization in `urls`.

The host needs KVM. VMs use a `/24` bridge with NAT (`network.subnet`,
default `10.231.0`), addresses `.10` to `.254`, and vsock CIDs 3 and up.

```sh
sudo journalctl -u microvm-scaleset
```

A controller restart kills every running VM, so deploys leave
`microvm-scaleset` running: a changed guest image, controller or flag takes
effect at its next restart. Restart it when the host is idle, or accept that
its running jobs fail:

```sh
sudo systemctl restart microvm-scaleset
```

This holds for the deploy that moves a host from QEMU to cloud-hypervisor
too: the new unit says not to restart, so the old QEMU controller keeps
running against slots that no longer exist. Restart `microvm-scaleset` once
after that first deploy; no cloud-hypervisor VM exists yet, so it costs
nothing.

## Legacy QEMU backend

`backend = "qemu"` keeps the earlier fixed pool: `poolSize` QEMU VMs declared
through microvm.nix, each sized by `vm.memGiB`, `vm.vcpu` and `vm.diskGiB`,
admitted against `memReserveGiB` and `memCommitRatio`, with optional balloon
reclamation (`balloon.*`). These options apply only to it; setting them with
the default backend fails evaluation.

```nix
services.microvm-github-runners = {
  backend = "qemu";
  poolSize = 8;
  vm = {
    memGiB = 10;
    initialMemGiB = 10;    # lower to start smaller and grow through QMP
    vcpu = 8;
    diskGiB = 100;
    cpuWeight = 100;       # elastic CPU sharing under contention
    cpuQuotaPercent = null; # optional hard ceiling; 400 permits four CPUs
  };
  memReserveGiB = 8;
  memCommitRatio = 1.0;    # 2.0 permits 200% of usable host RAM
  balloon = {
    enable = false;        # deploy telemetry first, then enable a canary
    floorGiB = 8;
    headroomGiB = 2;
    canarySlot = null;     # e.g. "my-host-1" during rollout
  };
};
```

The controller writes `/run/microvm-scaleset/status.json` with host headroom,
commitment, the admission gate and per-guest balloon statistics; completed
runners log a `runner memory summary`. Start with reclamation disabled, check
ownership recovery across a controller restart, then enable it for one slot
with `balloon.canarySlot`. Avoid cgroup `MemoryMax` below a VM's boot ceiling:
it can kill the VM instead of reclaiming guest cache.

## Status

Early, but running real CI. The scale set client is a public preview, so its
API may still change.
