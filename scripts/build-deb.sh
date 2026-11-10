#!/bin/sh
set -eu
arch=${1:-amd64}
case "$arch" in amd64|arm64) ;; *) exit 1 ;; esac
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"
mkdir -p dist
if [ ! -d vendor ]; then tar -xzf packaging/vendor.tar.gz; fi
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT HUP INT TERM
mkdir -p "$stage/DEBIAN" "$stage/usr/bin" "$stage/usr/lib/happy" "$stage/lib/systemd/system" "$stage/usr/share/doc/happy"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -mod=vendor -trimpath -ldflags='-s -w' -o "$stage/usr/bin/happy" ./cmd/happy
sh scripts/fetch-xray.sh "$arch" "$stage/usr/lib/happy"
cp packaging/happy.service "$stage/lib/systemd/system/"
cp README.md "$stage/usr/share/doc/happy/"
cat > "$stage/DEBIAN/control" <<CONTROL
Package: happy-gateway
Version: 0.1.0
Architecture: $arch
Maintainer: Happy Gateway maintainers
Depends: ca-certificates, adduser
Section: net
Priority: optional
Description: Personal WireGuard gateway with provider subscriptions and Xray
CONTROL
cat > "$stage/DEBIAN/postinst" <<'POSTINST'
#!/bin/sh
set -eu
if [ "$1" = configure ]; then
 getent passwd happy >/dev/null || adduser --system --group --home /var/lib/happy --no-create-home happy
 install -d -m 0700 -o happy -g happy /var/lib/happy
 install -d -m 0750 -o root -g happy /etc/happy
 if [ -d /run/systemd/system ]; then systemctl daemon-reload; fi
fi
POSTINST
cat > "$stage/DEBIAN/prerm" <<'PRERM'
#!/bin/sh
set -eu
if [ -d /run/systemd/system ]; then systemctl stop happy.service || true; fi
PRERM
chmod 755 "$stage/DEBIAN/postinst" "$stage/DEBIAN/prerm"
dpkg-deb --root-owner-group --build "$stage" "dist/happy-gateway_0.1.0_${arch}.deb"
