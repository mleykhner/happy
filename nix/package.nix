{ lib, stdenv, go, makeWrapper, xray }:
stdenv.mkDerivation {
  pname = "happy-gateway";
  version = "0.1.0";
  src = lib.cleanSourceWith {
    src = ../.;
    filter = path: type:
      let base = builtins.baseNameOf path;
      in !(builtins.elem base [ "node_modules" ".git" "data" "secrets" "bin" "result" "vendor" ])
        && path != toString ../dist;
  };
  nativeBuildInputs = [ go makeWrapper ];
  buildPhase = ''
    runHook preBuild
    export HOME=$TMPDIR GOCACHE=$TMPDIR/go-cache GOPROXY=off GOSUMDB=off CGO_ENABLED=0
    tar -xzf packaging/vendor.tar.gz
    go build -mod=vendor -trimpath -ldflags="-s -w" -o happy ./cmd/happy
    runHook postBuild
  '';
  doCheck = true;
  checkPhase = ''
    runHook preCheck
    go test -mod=vendor ./...
    runHook postCheck
  '';
  installPhase = ''
    mkdir -p $out/bin
    cp happy $out/bin/happy
    wrapProgram $out/bin/happy --set HAPPY_XRAY ${xray}/bin/xray
  '';
  meta = { description = "Personal WireGuard to Xray gateway"; mainProgram = "happy"; platforms = [ "x86_64-linux" "aarch64-linux" ]; };
}
