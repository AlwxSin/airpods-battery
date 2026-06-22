# Maintainer: Aleksandr Sinichkin <aleksandr.sinichkin@flant.com>
pkgname=airpods-battery-git
pkgver=r2.5b1f0b1
pkgrel=1
pkgdesc='AirPods battery daemon for Linux via BlueZ BatteryProvider1'
arch=('x86_64')
url='https://github.com/alwx/airpods-battery'
license=('MIT')
depends=('bluez' 'upower')
makedepends=('go' 'git')
provides=('airpods-battery')
conflicts=('airpods-battery')
source=("$pkgname::git+$url.git")
sha256sums=('SKIP')

pkgver() {
    cd "$pkgname"
    printf "r%s.%s" "$(git rev-list --count HEAD)" "$(git rev-parse --short HEAD)"
}

build() {
    cd "$pkgname"
    export CGO_ENABLED=0
    go build -trimpath -mod=readonly \
        -ldflags="-s -w" \
        -o airpods-battery .
}

package() {
    cd "$pkgname"
    install -Dm755 airpods-battery "$pkgdir/usr/bin/airpods-battery"
    install -Dm644 install/airpods-battery.service \
        "$pkgdir/usr/lib/systemd/user/airpods-battery.service"
    install -Dm644 LICENSE "$pkgdir/usr/share/licenses/$pkgname/LICENSE"
}
