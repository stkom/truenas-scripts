# Fan controller

Dual-zone Supermicro IPMI fan controller for TrueNAS SCALE. CPU duty follows package temperature. Drive duty uses the legacy `spinpid2` proportional/derivative calculation over mean temperature. The derivative uses the measured time between samples and is zero for the first sample after startup, a drive failure, or all drives in standby.

## Safety

- Invalid CPU reading sets the CPU zone to `safety.failure_duty`.
- Any configured drive with an invalid reading sets the drive zone to `safety.failure_duty`.
- Persistent fan RPM mismatch sets the affected zone to `safety.failure_duty` until a later check passes.
- RPM checks only compare against a duty that has been stable for `rpm_settle_time`. If the duty keeps changing, as the CPU zone can, the check is skipped and logged instead of failing.
- IPMI read failures during the CPU cycle set both zones to `safety.failure_duty` until the next successful cycle.
- Failure duty overrides the control target without replacing it. Once every fault on a zone clears, control resumes from its previous target.
- Standby drives are not woken. If every drive is in standby, drive duty becomes its configured minimum.
- SIGINT/SIGTERM restores `restore_mode`, or the BMC mode present at startup when `restore_mode` is `null`. If no automatic mode can be restored, both zones are set to `safety.failure_duty`.
- A panic in the drive loop stops the controller through the normal shutdown path, so the fan mode is still restored.
- `--dry-run` reads sensors and logs the duties it would set, but skips all IPMI writes. It also skips fan mode checks, duty reconciliation and RPM verification, because the BMC keeps controlling the fans.
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

Set `restore_mode` to the automatic Supermicro mode to return to on exit (0 Standard, 2 Optimal, 4 Heavy IO). Without it, the controller restores the mode it found at startup. After a crash and restart that mode is already manual, so a later clean exit leaves the fans at failure duty.

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
