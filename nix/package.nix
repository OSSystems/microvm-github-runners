{
  pkgs,
  inputs ? null,
  vmkit ? inputs.vmkit,
  ...
}:

pkgs.buildGoModule {
  pname = "microvm-scaleset";
  version = "0.1.0";
  src = pkgs.lib.fileset.toSource {
    root = ../.;
    fileset = pkgs.lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../cmd
    ];
  };
  # The Go packages come from the vmkit the flake locks, the same revision
  # as its NixOS module.
  postPatch = ''
    cp -r --no-preserve=mode ${vmkit} vmkit
    go mod edit -replace github.com/OSSystems/vmkit-nix=./vmkit
  '';
  subPackages = [ "cmd/microvm-scaleset" ];
  vendorHash = "sha256-F7clMjW+1wPPBhjM4CCP5DR0ps8LRkI3S4SN828ZuJU=";
  env.CGO_ENABLED = 0;
  meta = {
    description = "GitHub Actions runner scale set that runs each job in a throwaway microVM";
    license = pkgs.lib.licenses.mit;
    mainProgram = "microvm-scaleset";
  };
}
