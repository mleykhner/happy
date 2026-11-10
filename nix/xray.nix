{ lib, stdenvNoCC, fetchurl, unzip }:
let
  releases = {
    x86_64-linux = { archive = "Xray-linux-64.zip"; hash = "sha256-tlH34dmWbuLBse4b+LUscncYJNMGXc6Wu8i8wZ7ZX4I="; };
    aarch64-linux = { archive = "Xray-linux-arm64-v8a.zip"; hash = "sha256-ZO1oWJgqf164XYi4EQiQY71PF1YfTI9E4PHcjm9kML4="; };
  };
  release = releases.${stdenvNoCC.hostPlatform.system};
in stdenvNoCC.mkDerivation {
  pname = "happy-xray";
  version = "26.11.9";
  src = fetchurl {
    url = "https://github.com/XTLS/Xray-core/releases/download/v26.11.9/${release.archive}";
    inherit (release) hash;
  };
  nativeBuildInputs = [ unzip ];
  dontUnpack = true;
  installPhase = ''
    mkdir -p $out/bin $out/share/licenses/xray
    unzip -p $src xray > $out/bin/xray
    chmod +x $out/bin/xray
    unzip -p $src LICENSE > $out/share/licenses/xray/LICENSE
  '';
  meta = { platforms = builtins.attrNames releases; license = lib.licenses.mpl20; };
}
