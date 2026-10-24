{ lib, buildGoModule }:

buildGoModule {
  pname = "microvm-scaleset";
  version = "0.1.0";
  src = ./cmd/microvm-scaleset;
  vendorHash = "sha256-4Sy0Y/IXPhEwSlLX1CTECs9hISt+ao7wB8CddmJ5Qik=";
  env.CGO_ENABLED = 0;
  meta = {
    description = "GitHub Actions runner scale set that runs each job in a throwaway microVM";
    license = lib.licenses.mit;
    mainProgram = "microvm-scaleset";
  };
}
