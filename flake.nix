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
    { nixpkgs, microvm, ... }:
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

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.nixfmt);
    };
}
