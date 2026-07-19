#!/usr/bin/env python3

"""
spinpid2.py — Linux Python port of spinpid2.sh (dual fan zones)

This script regulates two IPMI fan zones (CPU and Peripheral) using
drive temperatures (PID-like PD control) and CPU temperature (linear scaling).

It targets Linux and ipmitool-capable BMCs (e.g., Supermicro X9/X10/X11/X12).

References:
- Original FreeBSD shell script spinpid2.sh and spinpid2.config
- hybrid_fan_controller.pl (Supermicro raw IPMI sequences)

Usage:
  python3 spinpid2.py --config spinpid2.ini

Config format: INI (see spinpid2.ini next to this script for an example)

Notes:
- Requires: ipmitool, smartmontools (smartctl), optionally lm-sensors (sensors)
- Run with sufficient privileges to access smartctl and ipmitool.
"""

import argparse
import configparser
import json
import logging
import signal
import subprocess
import sys
import time
from dataclasses import dataclass
from pathlib import Path
from typing import List, Optional, Tuple


@dataclass
class FanConfig:
    ipmitool: str
    zone_cpu: int
    zone_per: int
    duty_cpu_min: int
    duty_cpu_max: int
    duty_per_min: int
    duty_per_max: int
    rpm_cpu_30: int
    rpm_cpu_max: int
    rpm_per_30: int
    rpm_per_max: int
    sp: int
    drive_t_minutes: int
    kp: float
    kd: float
    cpu_t_seconds: int
    cpu_ref: int
    cpu_scale: int
    devices: List[str]
    log_path: Optional[Path]
    console: bool
    dry_run: bool


def clamp(val: int, lo: int, hi: int) -> int:
    return max(lo, min(hi, val))


def read_config(path: Path) -> FanConfig:
    cp = configparser.ConfigParser()
    with path.open() as f:
        cp.read_file(f)

    devices_raw = cp.get("drives", "devices", fallback="").strip()
    devices = [d.strip() for d in devices_raw.split(",") if d.strip()]

    log_path = cp.get("output", "log", fallback="").strip() or None
    log_path = Path(log_path) if log_path else None

    return FanConfig(
        ipmitool=cp.get("ipmi", "ipmitool_path", fallback="ipmitool"),
        zone_cpu=cp.getint("fans", "zone_cpu", fallback=0),
        zone_per=cp.getint("fans", "zone_per", fallback=1),
        duty_cpu_min=cp.getint("fans", "duty_cpu_min", fallback=20),
        duty_cpu_max=cp.getint("fans", "duty_cpu_max", fallback=100),
        duty_per_min=cp.getint("fans", "duty_per_min", fallback=20),
        duty_per_max=cp.getint("fans", "duty_per_max", fallback=100),
        rpm_cpu_30=cp.getint("fans", "rpm_cpu_30", fallback=400),
        rpm_cpu_max=cp.getint("fans", "rpm_cpu_max", fallback=3200),
        rpm_per_30=cp.getint("fans", "rpm_per_30", fallback=600),
        rpm_per_max=cp.getint("fans", "rpm_per_max", fallback=1700),
        sp=cp.getint("drives", "setpoint_c", fallback=36),
        drive_t_minutes=cp.getint("drives", "interval_minutes", fallback=5),
        kp=cp.getfloat("drives", "kp", fallback=4.0),
        kd=cp.getfloat("drives", "kd", fallback=40.0),
        cpu_t_seconds=cp.getint("cpu", "interval_seconds", fallback=5),
        cpu_ref=cp.getint("cpu", "ref_c", fallback=54),
        cpu_scale=cp.getint("cpu", "scale_per_c", fallback=6),
        devices=devices,
        log_path=log_path,
        console=cp.getboolean("output", "console", fallback=True),
        dry_run=cp.getboolean("output", "dry_run", fallback=False),
    )


def configure_logger(log_path: Optional[Path], console: bool) -> logging.Logger:
    """Configure and return a logger for this module using Python's logging.

    - If console is True, add a StreamHandler to stdout.
    - If log_path is provided, add a FileHandler (create parent dirs as needed).
    """
    logger = logging.getLogger("spinpid2")
    logger.setLevel(logging.INFO)

    # Avoid duplicating handlers if configure_logger is called more than once
    if logger.handlers:
        return logger

    formatter = logging.Formatter(
        fmt="%(asctime)s %(levelname)s: %(message)s",
        datefmt="%Y-%m-%d %H:%M:%S",
    )

    if console:
        sh = logging.StreamHandler(stream=sys.stdout)
        sh.setLevel(logging.INFO)
        sh.setFormatter(formatter)
        logger.addHandler(sh)

    if log_path:
        log_path.parent.mkdir(parents=True, exist_ok=True)
        fh = logging.FileHandler(log_path, encoding="utf-8")
        fh.setLevel(logging.INFO)
        fh.setFormatter(formatter)
        logger.addHandler(fh)

    return logger


def run_cmd(cmd: List[str]) -> Tuple[int, str, str]:
    p = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    out, err = p.communicate()
    return p.returncode, out, err


class IPMI:
    def __init__(self, ipmitool: str, logger: logging.Logger, dry_run: bool = False):
        self.ipmitool = ipmitool
        self.log = logger
        self.dry = dry_run

    def raw(self, *args: str) -> Tuple[int, str, str]:
        cmd = [self.ipmitool, "raw", *args]
        if self.dry:
            self.log.info(f"[DRY] ipmitool raw {' '.join(args)}")
            return 0, "", ""
        return run_cmd(cmd)

    def sdr(self) -> str:
        rc, out, err = run_cmd([self.ipmitool, "sdr"]) if not self.dry else (0, "", "")
        if rc != 0:
            raise RuntimeError(f"ipmitool sdr failed: {err.strip()}")
        return out

    def sensor_cpu_temp(self) -> Optional[int]:
        rc, out, err = run_cmd([self.ipmitool, "sensor", "get", "CPU Temp"]) if not self.dry else (0, "", "")
        if rc == 0 and out:
            for line in out.splitlines():
                if "Sensor Reading" in line:
                    parts = line.split()
                    # e.g., Sensor Reading  : 38 (+/- 0) degrees C
                    for tok in parts:
                        if tok.isdigit():
                            try:
                                return int(tok)
                            except ValueError:
                                pass
        return None

    def get_fan_mode(self) -> int:
        # 0x30 0x45 0 -> get fan mode
        rc, out, err = self.raw("0x30", "0x45", "0x00")
        if rc != 0:
            raise RuntimeError(f"get fan mode failed: {err}")
        # ipmitool prints hex like: 02
        out = out.strip().replace("\n", "")
        try:
            return int(out, 16) if out else 0
        except ValueError:
            return 0

    def set_fan_mode(self, mode: int) -> None:
        # 0x30 0x45 0x01 <mode>
        m = f"0x{mode:x}"
        rc, _, err = self.raw("0x30", "0x45", "0x01", m)
        if rc != 0:
            raise RuntimeError(f"set fan mode failed: {err}")

    def get_zone_duty(self, zone: int) -> Optional[int]:
        # 0x30 0x70 0x66 0 <zone>
        z = f"0x{zone:x}"
        rc, out, err = self.raw("0x30", "0x70", "0x66", "0x00", z)
        if rc != 0:
            return None
        out = out.strip().split()
        if not out:
            return None
        try:
            return int(out[0], 16)
        except ValueError:
            return None

    def set_zone_duty(self, zone: int, duty_percent: int) -> None:
        # 0x30 0x70 0x66 1 <zone> <duty 0-100>
        d = max(0, min(100, duty_percent))
        if self.dry:
            self.log.info(f"[DRY] set zone {zone} duty {d}%")
            return
        rc, _, err = self.raw("0x30", "0x70", "0x66", "0x01", f"0x{zone:x}", f"0x{d:x}")
        if rc != 0:
            raise RuntimeError(f"set zone duty failed: {err}")


def cpu_temp() -> Optional[int]:
    # Use lm-sensors JSON output and discard stderr (equivalent to: sensors -j 2>/dev/null)
    # Fixed path: coretemp-isa-0000 -> "Package id 0" -> temp1_input
    try:
        p = subprocess.Popen([
            "sensors",
            "-j",
        ], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
        out, _ = p.communicate()
        if p.returncode != 0 or not out:
            return None

        data = json.loads(out)

        chip = data.get("coretemp-isa-0000")
        if not isinstance(chip, dict):
            return None
        pkg = chip.get("Package id 0")
        if not isinstance(pkg, dict):
            return None

        # Prefer exact key temp1_input, but allow any temp*_input under the package block as fallback
        val = pkg.get("temp1_input")
        if val is None:
            for k, v in pkg.items():
                if isinstance(k, str) and k.startswith("temp") and k.endswith("_input"):
                    val = v
                    break
        if val is None:
            return None

        try:
            return int(float(val))
        except Exception:
            return None
    except Exception:
        return None


def smartctl_temp(dev: str) -> Optional[int]:
    rc, out, err = run_cmd(["smartctl", "-aj", dev])
    if rc != 0 and rc != 4:
        return None

    data = json.loads(out) if out else {}
    t = data.get("temperature", {}).get("current")
    if not isinstance(t, (int, float)):
        return None

    return int(float(t))


def main() -> int:
    ap = argparse.ArgumentParser(description="Dual-zone IPMI fan controller")
    ap.add_argument("--config", "-c", type=Path, default=Path(__file__).with_suffix(".ini"))
    args = ap.parse_args()

    cfg = read_config(args.config)
    logger = configure_logger(cfg.log_path, cfg.console)
    ipmi = IPMI(cfg.ipmitool, logger, dry_run=cfg.dry_run)

    # State
    last_errc: Optional[float] = None
    duty_cpu = cfg.duty_cpu_min
    duty_per = cfg.duty_per_min
    last_drive_check = 0.0

    # Ensure fan mode: manual (Supermicro uses raw 0x30 commands regardless of mode, but set Standard first)
    original_mode: Optional[int] = None
    try:
        original_mode = ipmi.get_fan_mode()
    except Exception:
        original_mode = None

    def restore_mode(_sig=None, _frame=None):
        try:
            if original_mode is not None:
                ipmi.set_fan_mode(original_mode)
        except Exception:
            pass
        sys.exit(0)

    signal.signal(signal.SIGINT, restore_mode)
    signal.signal(signal.SIGTERM, restore_mode)

    # Set "Standard" to let raw zone control work and give BMC room to breathe
    try:
        ipmi.set_fan_mode(0)
    except Exception as ex:
        logger.warning(f"Unable to set fan mode Standard: {ex}")

    logger.info("spinpid2.py started")

    while True:
        tnow = time.time()

        # CPU temperature influence (linear scaling)
        cpu_temp = ipmi.sensor_cpu_temp() or cpu_temp()
        if cpu_temp is not None:
            duty_cpu = cfg.duty_cpu_min + (cpu_temp - cfg.cpu_ref) * cfg.cpu_scale
            duty_cpu = clamp(int(duty_cpu), cfg.duty_cpu_min, cfg.duty_cpu_max)
        else:
            duty_cpu = cfg.duty_cpu_min

        # Run drive check every DRIVE_T minutes
        pd_term = 0
        tmean = None
        tmax = None
        if tnow - last_drive_check >= cfg.drive_t_minutes * 60:
            last_drive_check = tnow

            temps: List[int] = []
            for dev in cfg.devices:
                temp = smartctl_temp(dev)
                if temp is not None:
                    temps.append(temp)

            if temps:
                tmax = max(temps)
                tmean = sum(temps) / len(temps)
                err = tmean - cfg.sp
                derr = 0.0 if last_errc is None else (err - last_errc)
                pd = cfg.kp * err + cfg.kd * derr
                last_errc = err
                pd_term = int(pd)

                # Adjust duty for peripheral zone (drives)
                duty_per = clamp(int((duty_per if isinstance(duty_per, int) else cfg.duty_per_min) + pd_term),
                                 cfg.duty_per_min, cfg.duty_per_max)
            else:
                # No spinning drives
                duty_per = cfg.duty_per_min
                last_errc = None

            # Log summary line
            s_cpu = f"CPU {cpu_temp if cpu_temp is not None else '?'}C"
            s_drv = f"Drives Tmax={tmax if tmax is not None else '-'}C Tmean={int(tmean) if tmean is not None else '-'}C"
            logger.info(f"{s_cpu} {s_drv} -> duty CPU={duty_cpu}% PER={duty_per}%")

        # Decide final duties and write if changed from current
        # Optionally take max of CPU demand and PER demand for CPU zone; keep them separate as in original script
        try:
            current_cpu = ipmi.get_zone_duty(cfg.zone_cpu)
            current_per = ipmi.get_zone_duty(cfg.zone_per)
        except Exception:
            current_cpu = None
            current_per = None

        if current_cpu is None or current_cpu != duty_cpu:
            try:
                ipmi.set_zone_duty(cfg.zone_cpu, duty_cpu)
            except Exception as ex:
                logger.error(f"Error setting CPU zone duty: {ex}")

        if current_per is None or current_per != duty_per:
            try:
                ipmi.set_zone_duty(cfg.zone_per, duty_per)
            except Exception as ex:
                logger.error(f"Error setting PER zone duty: {ex}")

        time.sleep(cfg.cpu_t_seconds)

    # unreachable
    # restore_mode()
    # return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except SystemExit as e:
        raise
    except Exception as e:
        sys.stderr.write(f"spinpid2.py fatal error: {e}\n")
        sys.exit(1)
