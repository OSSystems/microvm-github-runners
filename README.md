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
in every organization listed in `urls` and tells GitHub how many jobs the host
can take right now. All scale sets share one pool of VMs: an idle organization
holds no slots, and when several have jobs waiting, free VMs go to them in
turn. It boots one VM from the pool per assigned job, and grows only while:

- the 1-minute load average per CPU is below `maxLoadPerCpu`,
- committed allocations stay within `(host RAM - memReserveGiB) * memCommitRatio`, and
- available host memory covers another VM, the host reserve, and reserved VM growth.

`memCommitRatio = 1.0` permits 100% of usable host RAM; `2.0` permits 200%.
The deprecated `memOvercommit = x` setting maps to `memCommitRatio = 1 + x`.
Sixteen slots do not guarantee sixteen concurrent jobs: memory and CPU limits
can hold the running count below the slot count.

Free page reporting returns unused guest pages to the host, and VM disks
bypass the host's page cache. Guest filesystem caches can still retain RAM.
Optional balloon reclamation releases that cache under pressure while keeping
one shared pool for light and heavy jobs. It keeps each VM between
`balloon.floorGiB` and its original boot ceiling, `vm.memGiB`, with
`balloon.headroomGiB` available inside the guest.

Set `vm.initialMemGiB` below `vm.memGiB` to start smaller. The guest runner
waits until the controller confirms that initial allocation through QMP.
Each unready start reserves its initial allocation, and its ready marker is
cleared before reuse. Parallel starts remain subject to the host budget.
Admission budgets committed allocations and RAM already granted but not yet
resident, rather than reserving every guest’s full ceiling. Established elastic
guests grow on guest pressure instead of expanding merely because the host is idle.
The ceiling is a limit, not a promise that every guest can reach it simultaneously.

Reclamation requires fresh guest statistics, waits 90 seconds after boot,
and shrinks by at most 256 MiB every 30 seconds. Growth also requires host
headroom, including when reclamation is disabled. Stale statistics freeze
target changes. An observed guest OOM or emergency balloon deflation disables
further reclamation for that boot. Emergency deflation can consume host
headroom, so overcommit remains a tradeoff rather than a hard reserve guarantee.

Ownership is saved before each boot, so a controller restart restores the
organization, runner name, boot ceiling, and pending-start reservation. For
older guests, the controller confirms the JIT runner identity against each
organization before adopting it. An unresolved active owner inhibits new boots;
the controller does not stop that VM or replace its credentials.

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
    urls = [                        # all share the VM pool
      "https://github.com/my-org"
      "https://github.com/my-other-org"
    ];
    name = "my-microvm";            # workflows use `runs-on: my-microvm`
    githubApp = {
      id = "123456";
      privateKeyFile = "/run/secrets/github-app-key";
    };
    network.externalInterface = "enp6s0";
    poolSize = 8;
    vm = {
      memGiB = 10;
      vcpu = 8;
      diskGiB = 100;
      cpuWeight = 100;       # elastic CPU sharing under contention
      cpuQuotaPercent = null; # optional hard ceiling; 400 permits four CPUs
    };
    memReserveGiB = 8;
    memCommitRatio = 1.0;
    balloon = {
      enable = false;       # deploy telemetry first, then enable a canary
      floorGiB = 8;
      headroomGiB = 2;
      canarySlot = null;     # e.g. "my-microvm-1" during rollout
    };

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
permission and must be installed on every organization in `urls`.

The host needs KVM. VMs use QEMU, a `/24` bridge with NAT (`network.subnet`,
default `10.231.0`) and vsock CIDs 3 and up.

## Observe and tune

The controller writes `/run/microvm-scaleset/status.json` with host headroom,
nominal commitment, the current admission gate, organization counts, and
per-guest balloon statistics, cgroup memory, swap, and observed peaks. Completed
runners emit a `runner memory summary` journal entry so observed peaks remain
available after a slot is reused. These are sampled values, not guaranteed peaks.

```sh
sudo jq . /run/microvm-scaleset/status.json
sudo journalctl -u microvm-scaleset --grep='runner memory summary'
```

Start with reclamation disabled. Check ownership recovery across a controller
restart, then enable it for one slot with `balloon.canarySlot`. Confirm that
the VM cgroup's memory falls after reclamation and that guest OOM counts do not
rise before widening the rollout. Changing VM sizing only affects later boots;
deployment does not restart running guests.

`cpuWeight` permits bursts when other work is idle. `cpuQuotaPercent` caps the
VM process and can be changed through systemd without rebooting the guest.
Host-side Nix builds and virtiofs processes run in separate services and are
not included in that per-VM CPU quota. Avoid cgroup `MemoryMax` below a VM's
boot ceiling: it can kill the VM instead of reclaiming guest cache.

## Status

Early, but running real CI. The scale set client is a public preview, so its
API may still change.
