{
  description = "Happy personal WireGuard gateway";
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      eachSystem = nixpkgs.lib.genAttrs systems;
    in {
      packages = eachSystem (system:
        let
          pkgs = import nixpkgs { inherit system; };
          xray = pkgs.callPackage ./nix/xray.nix { };
          happy = pkgs.callPackage ./nix/package.nix { inherit xray; };
        in { default = happy; inherit happy xray; });
      nixosModules.default = import ./nix/module.nix self;
    };
}
