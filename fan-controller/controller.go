package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"
)

type DriveControlResult struct {
	Duty         int
	Mean         float64
	Maximum      float64
	Active       int
	Standby      int
	Failed       int
	Error        float64
	Proportional float64
	Derivative   float64
}

type Controller struct {
	cfg       *Config
	ipmi      *IPMIClient
	smartctl  *SmartctlClient
	log       *slog.Logger
	cpuDuty   int
	driveDuty int
	driveErr  float64
	oldMode   int
	haveMode  bool
}

func NewController(cfg *Config, ipmi *IPMIClient, smartctl *SmartctlClient, logger *slog.Logger) *Controller {
	return &Controller{cfg: cfg, ipmi: ipmi, smartctl: smartctl, log: logger}
}

func (controller *Controller) Run(ctx context.Context, once bool) error {
	if err := controller.start(ctx); err != nil {
		return err
	}
	defer controller.restoreMode()

	if err := controller.updateCPU(ctx); err != nil {
		controller.log.Error("CPU update failed", "event", "cpu_update_failed", "error", err)
	}
	if err := controller.updateDrives(ctx); err != nil {
		controller.log.Error("drive update failed", "event", "drive_update_failed", "error", err)
	}
	if err := controller.verifyFanRPMs(ctx); err != nil {
		controller.log.Error("fan RPM verification failed", "event", "rpm_verification_failed", "error", err)
	}
	if once {
		return nil
	}

	cpuTicker := time.NewTicker(controller.cfg.CPU.PollInterval.Duration)
	driveTicker := time.NewTicker(controller.cfg.Drives.PollInterval.Duration)
	defer cpuTicker.Stop()
	defer driveTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-cpuTicker.C:
			if err := controller.updateCPU(ctx); err != nil {
				controller.log.Error("CPU update failed", "event", "cpu_update_failed", "error", err)
			}
			if err := controller.reconcileDuties(ctx); err != nil {
				controller.log.Error("fan duty reconciliation failed", "event", "duty_reconciliation_failed", "error", err)
			}
		case <-driveTicker.C:
			if err := controller.ensureManualMode(ctx); err != nil {
				controller.log.Error("fan mode verification failed", "event", "fan_mode_failed", "error", err)
				controller.useFailureDuties(ctx, "fan mode verification failure")
				continue
			}
			if err := controller.updateDrives(ctx); err != nil {
				controller.log.Error("drive update failed", "event", "drive_update_failed", "error", err)
			}
			if err := controller.verifyFanRPMs(ctx); err != nil {
				controller.log.Error("fan RPM verification failed", "event", "rpm_verification_failed", "error", err)
			}
		}
	}
}

func (controller *Controller) start(ctx context.Context) error {
	mode, err := controller.ipmi.GetFanMode(ctx)
	if err != nil {
		return fmt.Errorf("error reading initial fan mode: %w", err)
	}
	controller.oldMode = mode
	controller.haveMode = true
	if mode != controller.cfg.ManualMode {
		if err := controller.ipmi.SetFanMode(ctx, controller.cfg.ManualMode); err != nil {
			return fmt.Errorf("error enabling manual fan mode: %w", err)
		}
		if err := waitFor(ctx, controller.cfg.ModeSettleTime.Duration); err != nil {
			return fmt.Errorf("error waiting for manual fan mode: %w", err)
		}
	}

	controller.cpuDuty = clampDuty(controller.cfg.InitialDuty, controller.cfg.CPU.Fan)
	controller.driveDuty = clampDuty(controller.cfg.InitialDuty, controller.cfg.Drives.Fan)
	if err := controller.ipmi.SetZoneDuty(ctx, controller.cfg.CPU.Fan.Zone, controller.cpuDuty); err != nil {
		return controller.startFailure(ctx, fmt.Errorf("error setting initial CPU duty: %w", err))
	}
	if err := controller.ipmi.SetZoneDuty(ctx, controller.cfg.Drives.Fan.Zone, controller.driveDuty); err != nil {
		return controller.startFailure(ctx, fmt.Errorf("error setting initial drive duty: %w", err))
	}
	controller.log.Info("fan controller started",
		"event", "startup_completed",
		"old_mode", controller.oldMode,
		"manual_mode", controller.cfg.ManualMode,
		"cpu_duty", controller.cpuDuty,
		"drive_duty", controller.driveDuty,
		"drive_count", len(controller.cfg.Drives.Devices),
	)
	return nil
}

func (controller *Controller) startFailure(ctx context.Context, cause error) error {
	controller.setFailureDuty(ctx, controller.cfg.CPU.Fan, "startup")
	controller.setFailureDuty(ctx, controller.cfg.Drives.Fan, "startup")
	return cause
}

func (controller *Controller) restoreMode() {
	if !controller.cfg.RestoreModeOnExit || !controller.haveMode {
		return
	}
	if controller.oldMode == controller.cfg.ManualMode {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), controller.cfg.IPMI.Timeout.Duration)
	defer cancel()
	if err := controller.ipmi.SetFanMode(ctx, controller.oldMode); err != nil {
		controller.log.Error("fan mode restoration failed", "event", "fan_mode_restore_failed", "error", err)
		return
	}
	controller.log.Info("fan mode restored", "event", "fan_mode_restored", "mode", controller.oldMode)
}

func (controller *Controller) ensureManualMode(ctx context.Context) error {
	mode, err := controller.ipmi.GetFanMode(ctx)
	if err != nil {
		return err
	}
	if mode == controller.cfg.ManualMode {
		return nil
	}
	controller.log.Warn("fan mode changed externally", "event", "fan_mode_changed", "mode", mode)
	return controller.ipmi.SetFanMode(ctx, controller.cfg.ManualMode)
}

func (controller *Controller) reconcileDuties(ctx context.Context) error {
	if !controller.cfg.ReadDuty {
		return nil
	}
	cpuErr := controller.reconcileDuty(ctx, controller.cfg.CPU.Fan, controller.cpuDuty)
	driveErr := controller.reconcileDuty(ctx, controller.cfg.Drives.Fan, controller.driveDuty)
	if cpuErr != nil || driveErr != nil {
		controller.useFailureDuties(ctx, "fan duty reconciliation failure")
	}
	return errors.Join(cpuErr, driveErr)
}

func (controller *Controller) reconcileDuty(ctx context.Context, fan FanConfig, desired int) error {
	actual, err := controller.ipmi.GetZoneDuty(ctx, fan.Zone)
	if err != nil {
		return fmt.Errorf("error reading zone %d duty: %w", fan.Zone, err)
	}
	if actual == desired {
		return nil
	}
	controller.log.Warn("fan duty changed externally", "event", "duty_mismatch", "zone", fan.Zone, "actual", actual, "desired", desired)
	if err := controller.ipmi.SetZoneDuty(ctx, fan.Zone, desired); err != nil {
		return fmt.Errorf("error reapplying zone %d duty: %w", fan.Zone, err)
	}
	return nil
}

func (controller *Controller) updateCPU(ctx context.Context) error {
	temperature, source, err := controller.readCPUTemperature(ctx)
	if err != nil {
		setErr := controller.setFailureDuty(ctx, controller.cfg.CPU.Fan, "CPU sensor failure")
		if setErr == nil {
			controller.cpuDuty = controller.cfg.Safety.FailureDuty
		}
		return errors.Join(err, setErr)
	}
	duty := calculateCPUDuty(temperature, controller.cfg.CPU)
	if err := controller.setDutyOrFailSafe(ctx, controller.cfg.CPU.Fan, duty, &controller.cpuDuty); err != nil {
		return err
	}
	controller.log.Info("CPU fan updated", "event", "cpu_update", "temperature_c", temperature, "source", source, "duty", duty)
	return nil
}

func (controller *Controller) readCPUTemperature(ctx context.Context) (float64, string, error) {
	var hwmonErr error
	if controller.cfg.CPU.HwmonRoot != "" {
		temperature, err := ReadHwmonCPUTemperature(controller.cfg.CPU.HwmonRoot)
		if err == nil && validTemperature(temperature) {
			return temperature, "hwmon", nil
		}
		if err == nil {
			err = fmt.Errorf("hwmon returned invalid CPU temperature %.1f", temperature)
		}
		hwmonErr = err
	}
	if controller.cfg.CPU.IPMISensor != "" {
		temperature, err := controller.ipmi.ReadTemperature(ctx, controller.cfg.CPU.IPMISensor)
		if err == nil && validTemperature(temperature) {
			return temperature, "ipmi", nil
		}
		if err == nil {
			err = fmt.Errorf("IPMI returned invalid CPU temperature %.1f", temperature)
		}
		return 0, "", errors.Join(hwmonErr, err)
	}
	return 0, "", hwmonErr
}

func (controller *Controller) updateDrives(ctx context.Context) error {
	readings := ReadDriveTemperatures(ctx, controller.smartctl, controller.cfg.Drives.Devices)
	result := calculateDriveDuty(controller.driveDuty, controller.driveErr, readings, controller.cfg.Drives, controller.cfg.Safety.FailureDuty)
	controller.logDriveIssues(readings)
	if result.Failed > 0 {
		controller.driveErr = 0
	} else if result.Active > 0 {
		controller.driveErr = result.Error
	} else {
		controller.driveErr = 0
	}
	if err := controller.setDutyOrFailSafe(ctx, controller.cfg.Drives.Fan, result.Duty, &controller.driveDuty); err != nil {
		return err
	}
	controller.log.Info("drive fans updated",
		"event", "drive_update",
		"active", result.Active,
		"standby", result.Standby,
		"failed", result.Failed,
		"temperature_mean_c", result.Mean,
		"temperature_max_c", result.Maximum,
		"error", result.Error,
		"proportional", result.Proportional,
		"derivative", result.Derivative,
		"duty", result.Duty,
	)
	return nil
}

func (controller *Controller) logDriveIssues(readings []DriveReading) {
	for _, reading := range readings {
		if reading.Err != nil {
			controller.log.Error("drive temperature unavailable", "event", "drive_sensor_failed", "device", reading.Device, "error", reading.Err)
		}
		if reading.Warning != "" {
			controller.log.Warn("drive SMART warning", "event", "drive_smart_warning", "device", reading.Device, "warning", reading.Warning)
		}
	}
}

func (controller *Controller) setFailureDuty(ctx context.Context, fan FanConfig, reason string) error {
	controller.log.Error("using failure duty", "event", "failure_duty", "zone", fan.Zone, "duty", controller.cfg.Safety.FailureDuty, "reason", reason)
	return controller.ipmi.SetZoneDuty(ctx, fan.Zone, controller.cfg.Safety.FailureDuty)
}

func (controller *Controller) useFailureDuties(ctx context.Context, reason string) {
	if err := controller.setFailureDuty(ctx, controller.cfg.CPU.Fan, reason); err != nil {
		controller.log.Error("CPU failure duty failed", "event", "failure_duty_failed", "zone", controller.cfg.CPU.Fan.Zone, "error", err)
	} else {
		controller.cpuDuty = controller.cfg.Safety.FailureDuty
	}
	if err := controller.setFailureDuty(ctx, controller.cfg.Drives.Fan, reason); err != nil {
		controller.log.Error("drive failure duty failed", "event", "failure_duty_failed", "zone", controller.cfg.Drives.Fan.Zone, "error", err)
	} else {
		controller.driveDuty = controller.cfg.Safety.FailureDuty
	}
}

func (controller *Controller) setDutyOrFailSafe(ctx context.Context, fan FanConfig, desired int, current *int) error {
	if err := controller.setDuty(ctx, fan.Zone, desired, current); err != nil {
		failureErr := controller.setFailureDuty(ctx, fan, "fan duty write failure")
		if failureErr == nil {
			*current = controller.cfg.Safety.FailureDuty
		}
		return errors.Join(err, failureErr)
	}
	return nil
}

func (controller *Controller) setDuty(ctx context.Context, zone, desired int, current *int) error {
	if desired == *current {
		return nil
	}
	if err := controller.ipmi.SetZoneDuty(ctx, zone, desired); err != nil {
		return fmt.Errorf("error setting zone %d duty to %d: %w", zone, desired, err)
	}
	*current = desired
	return nil
}

func (controller *Controller) verifyFanRPMs(ctx context.Context) error {
	if !controller.cfg.CPU.Fan.RPMMonitor.Enabled && !controller.cfg.Drives.Fan.RPMMonitor.Enabled {
		return nil
	}
	if err := waitFor(ctx, controller.cfg.RPMSettleTime.Duration); err != nil {
		return err
	}
	cpuErr := controller.verifyFanRPM(ctx, controller.cfg.CPU.Fan, &controller.cpuDuty)
	driveErr := controller.verifyFanRPM(ctx, controller.cfg.Drives.Fan, &controller.driveDuty)
	return errors.Join(cpuErr, driveErr)
}

func (controller *Controller) verifyFanRPM(ctx context.Context, fan FanConfig, current *int) error {
	monitor := fan.RPMMonitor
	if !monitor.Enabled {
		return nil
	}
	for attempt := 0; ; attempt++ {
		rpm, err := controller.ipmi.ReadFanRPM(ctx, monitor.Sensor)
		if err != nil {
			return controller.fanRPMFailure(ctx, fan, current, fmt.Errorf("error reading %s RPM: %w", monitor.Sensor, err))
		}
		mismatch, reason := fanRPMMismatch(*current, rpm, monitor)
		controller.log.Info("fan RPM checked", "event", "rpm_check", "sensor", monitor.Sensor, "rpm", rpm, "duty", *current, "mismatch", mismatch)
		if !mismatch {
			return nil
		}
		if attempt >= controller.cfg.RPMRetries {
			return controller.fanRPMFailure(ctx, fan, current, errors.New(reason))
		}
		controller.log.Warn("fan RPM mismatch; reapplying duty", "event", "rpm_mismatch", "sensor", monitor.Sensor, "rpm", rpm, "duty", *current, "attempt", attempt+1, "reason", reason)
		if err := controller.ipmi.SetZoneDuty(ctx, fan.Zone, *current); err != nil {
			return controller.fanRPMFailure(ctx, fan, current, fmt.Errorf("error reapplying zone %d duty: %w", fan.Zone, err))
		}
		if err := waitFor(ctx, controller.cfg.RPMSettleTime.Duration); err != nil {
			return err
		}
	}
}

func (controller *Controller) fanRPMFailure(ctx context.Context, fan FanConfig, current *int, cause error) error {
	err := controller.setFailureDuty(ctx, fan, "fan RPM verification failure")
	if err == nil {
		*current = controller.cfg.Safety.FailureDuty
	}
	return errors.Join(cause, err)
}

func fanRPMMismatch(duty int, rpm float64, monitor RPMMonitorConfig) (bool, string) {
	if duty >= 95 {
		minimum := 0.8 * float64(monitor.RPMAt100)
		if rpm < minimum {
			return true, fmt.Sprintf("RPM %.0f below high-duty minimum %.0f", rpm, minimum)
		}
	}
	if duty < 25 {
		maximum := 1.2 * float64(monitor.RPMAt30)
		if rpm > maximum {
			return true, fmt.Sprintf("RPM %.0f above low-duty maximum %.0f", rpm, maximum)
		}
	}
	return false, ""
}

func waitFor(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func calculateCPUDuty(temperature float64, cfg CPUConfig) int {
	demand := float64(cfg.Fan.MinDuty) + (temperature-cfg.ReferenceC)*cfg.DutyPerDegree
	return clampDuty(int(math.Ceil(demand)), cfg.Fan)
}

func validTemperature(temperature float64) bool {
	return temperature >= -20 && temperature <= 150 && !math.IsNaN(temperature)
}

func calculateDriveDuty(current int, previousError float64, readings []DriveReading, cfg DriveConfig, failureDuty int) DriveControlResult {
	result := DriveControlResult{}
	var temperatures []float64
	for _, reading := range readings {
		switch reading.State {
		case DriveActive:
			result.Active++
			temperatures = append(temperatures, reading.Temperature)
		case DriveStandby:
			result.Standby++
		default:
			result.Failed++
		}
	}
	if result.Failed > 0 {
		result.Duty = failureDuty
		return result
	}
	if len(temperatures) == 0 {
		result.Duty = cfg.Fan.MinDuty
		return result
	}
	for _, temperature := range temperatures {
		result.Mean += temperature
	}
	result.Mean /= float64(len(temperatures))
	result.Maximum = maxFloat(temperatures)
	result.Error = result.Mean - cfg.SetpointC
	result.Proportional = cfg.Kp * result.Error
	minutes := cfg.PollInterval.Duration.Minutes()
	result.Derivative = cfg.Kd * (result.Error - previousError) / minutes
	change := int(math.Round(result.Proportional + result.Derivative))
	result.Duty = clampDuty(current+change, cfg.Fan)
	return result
}

func clampDuty(value int, cfg FanConfig) int {
	return max(cfg.MinDuty, min(cfg.MaxDuty, value))
}
