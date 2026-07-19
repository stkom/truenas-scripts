# Fan controller

Dual-zone Supermicro IPMI fan controller for TrueNAS SCALE. CPU duty follows package temperature. Drive duty uses the legacy `spinpid2` proportional/derivative calculation over mean temperature.

## Safety

- Invalid CPU reading sets the CPU zone to `safety.failure_duty`.
- Any configured drive with an invalid reading sets the drive zone to `safety.failure_duty`.
- Persistent fan RPM mismatch sets the affected zone to `safety.failure_duty`.
- Standby drives are not woken. If every drive is in standby, drive duty becomes its configured minimum.
- SIGINT/SIGTERM restores the BMC fan mode present at startup when `restore_mode_on_exit` is enabled.
- `--dry-run` reads hardware but skips all IPMI writes.
- With `read_duty` enabled, each CPU cycle detects and corrects BMC duty resets. This corresponds to legacy `HOW_DUTY=1`.

Test this against the exact motherboard and fan wiring before unattended use. Supermicro raw commands are board-specific.

## Build

```sh
go test ./...
CGO_ENABLED=0 go build -trimpath -o fan-controller .
```

Build on another Linux machine if Go is unavailable on TrueNAS. Copy only the binary and configuration to a persistent dataset.

## Configure

```sh
cp fan-controller.example.json fan-controller.json
./fan-controller -config fan-controller.json -check
```

List every controlled HDD explicitly in `drives.devices`. `smartctl -n standby` avoids waking sleeping drives. CPU temperature uses Linux hwmon first, then the configured IPMI sensor.

RPM calibration retains the old config values. CPU RPM monitoring defaults off, matching the disabled check in the working shell script. Automatic BMC cold reset is intentionally omitted.

The example uses the current NAS devices and a tested 30% CPU minimum. At 20%, FAN2 reached its 300 RPM critical threshold and the BMC repeatedly forced both zones to 100%.

`ipmi.args` supports additional global `ipmitool` arguments. Avoid command-line passwords; they can be exposed through process listings.

## Test and run

```sh
./fan-controller -config fan-controller.json -dry-run -once
./fan-controller -config fan-controller.json
```

The executable requires root access plus `ipmitool` and `smartctl`. Use the TrueNAS Init/Shutdown Scripts UI to launch the persistent binary after initialization.

Replace the old hybrid-controller post-init entry with:

```sh
tmux new-session -d -s fan-controller '/root/scripts/fan-controller -config /root/scripts/fan-controller.json'
```
