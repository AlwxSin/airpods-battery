# airpods-battery

A lightweight Linux daemon that exposes AirPods battery levels to UPower via the BlueZ BatteryProvider1 API. Once running, GNOME, KDE, and Waybar show AirPods battery the same way they show any other Bluetooth device.

**Note:** UPower expects a single battery percentage per device. The daemon reports the minimum value across both earbuds — the one that will run out first. The case is excluded.

Battery data is read directly from the headphones over an L2CAP connection using Apple's AAP protocol — the same approach used by [MagicPods](https://github.com/steam3d/MagicPodsLinux).

## Requirements

- Linux kernel with Bluetooth support
- BlueZ >= 5.56
- UPower

## Installation

### AUR (currently unavailable)

```
yay -S airpods-battery-git
```

After installation, enable the user service:

```
systemctl --user enable --now airpods-battery
```

### Manual

```
git clone https://github.com/alwx/airpods-battery
cd airpods-battery
go build -o airpods-battery .
sudo install -Dm755 airpods-battery /usr/bin/airpods-battery
mkdir -p ~/.config/systemd/user
cp install/airpods-battery.service ~/.config/systemd/user/
systemctl --user enable --now airpods-battery
```

## Usage

The daemon runs as a systemd user service and requires no configuration. Pair and connect your AirPods as usual — battery will appear in UPower within a few seconds.

```
upower -e | grep headphones
upower -i /org/freedesktop/UPower/devices/headphones_dev_XX_XX_XX_XX_XX_XX
```

Logs:

```
journalctl --user -u airpods-battery -f
```

## Supported devices

Any Apple headphones recognized by BlueZ as `audio-headphones` or `audio-headset` should work. Tested with AirPods 4 ANC. The following models have extended initialization support: AirPods Pro 2, AirPods Pro 3, AirPods Pro USB-C, AirPods 4 ANC, AirPods Max 2.

## How it works

1. Connects to the headphones over L2CAP PSM 0x1001 using Apple's AAP protocol.
2. Sends the AAP initialization sequence, then listens for battery packets.
3. Registers the parsed battery percentage with BlueZ via `BatteryProviderManager1.RegisterBatteryProvider`.
4. BlueZ propagates the value to UPower, which makes it available to the desktop environment.

When the headphones switch between A2DP and HFP (e.g., during calls), the daemon keeps the battery display alive by preferring the AAP data over BlueZ's native `Battery1`.

## License

MIT
