# Fan controller

Dual-zone Supermicro IPMI fan controller for TrueNAS SCALE. CPU duty follows package temperature. Drive duty uses the legacy `spinpid2` proportional/derivative calculation over mean temperature.

## Safety

- Invalid CPU reading sets the CPU zone to `safety.failure_duty`.
- Any configured drive with an invalid reading sets the drive zone to `safety.failure_duty`.
- Persistent fan RPM mismatch sets the affected zone to `safety.failure_duty` until a later check passes.
- IPMI read failures during the CPU cycle set both zones to `safety.failure_duty` until the next successful cycle.
- Failure duty overrides the control target without replacing it. Once every fault on a zone clears, control resumes from its previous target.
- Standby drives are not woken. If every drive is in standby, drive duty becomes its configured minimum.
- SIGINT/SIGTERM restores the automatic BMC mode present at startup. If no automatic mode can be restored, both zones are set to `safety.failure_duty`.
- `--dry-run` reads hardware but skips all IPMI writes.
- With `read_duty` enabled, each CPU cycle detects and corrects BMC duty resets. This corresponds to legacy `HOW_DUTY=1`.
- Each CPU cycle also restores externally changed fan mode, waits for the BMC, then forcibly reapplies both zone duties.
- Drive temperature and RPM checks run independently, so slow disks cannot delay CPU control.

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

List every controlled HDD explicitly in `drives.devices`, using stable `/dev/disk/by-id/` paths. `smartctl -n standby` avoids waking sleeping drives; both smartctl 7.4 text-backed and 7.5 structured power-mode JSON are supported. CPU temperature uses Linux hwmon first, then the configured IPMI sensor.

`cpu.minimum_temperature_c` and `drives.minimum_temperature_c` reject implausibly low numeric readings. Both default to 5°C.

Reference/setpoint temperatures must stay within the configured sensor range. Control gains are capped at 100; out-of-range values fail `-check`.

File logs rotate before exceeding `log_max_size_mb`. `log_backups` controls how many numbered backups are retained. Unchanged five-second CPU readings are not written at INFO level.

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
tmux new-session -d -s fan-controller 'until /root/scripts/fan-controller -config /root/scripts/fan-controller.json; do sleep 5; done'
```

The `until` loop restarts abnormal exits. A graceful SIGINT/SIGTERM exits normally and is not restarted.
