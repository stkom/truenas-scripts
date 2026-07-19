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

type FanHardware interface {
	GetFanMode(context.Context) (int, error)
	SetFanMode(context.Context, int) error
	GetZoneDuty(context.Context, int) (int, error)
	SetZoneDuty(context.Context, int, int) error
	ReadFanRPM(context.Context, string) (float64, error)
}

type TemperatureSensor interface {
	ReadTemperature(context.Context, string) (float64, error)
}

type CPUReader func(context.Context) (float64, string, error)
type DriveReader func(context.Context, []string) []DriveReading

func NewCPUReader(cfg CPUConfig, sensor TemperatureSensor) CPUReader {
	return func(ctx context.Context) (float64, string, error) {
		var hwmonErr error
		if cfg.HwmonRoot != "" {
			temperature, err := ReadHwmonCPUTemperature(cfg.HwmonRoot)
			if err == nil && validTemperature(temperature) {
				return temperature, "hwmon", nil
			}
			if err == nil {
				err = fmt.Errorf("hwmon returned invalid CPU temperature %.1f", temperature)
			}
			hwmonErr = err
		}
		if cfg.IPMISensor != "" {
			temperature, err := sensor.ReadTemperature(ctx, cfg.IPMISensor)
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
}

func NewDriveReader(client *SmartctlClient) DriveReader {
	return func(ctx context.Context, devices []string) []DriveReading {
		return ReadDriveTemperatures(ctx, client, devices)
	}
}

type controlledZone struct {
	name        string
	cfg         FanConfig
	duty        int
	failureDuty int
	hardware    FanHardware
	log         *slog.Logger
}

func newControlledZone(name string, cfg FanConfig, failureDuty int, hardware FanHardware, logger *slog.Logger) *controlledZone {
	return &controlledZone{name: name, cfg: cfg, failureDuty: failureDuty, hardware: hardware, log: logger}
}

func (zone *controlledZone) set(ctx context.Context, desired int) (bool, error) {
	if desired == zone.duty {
		return false, nil
	}
	if err := zone.force(ctx, desired); err != nil {
		return false, err
	}
	return true, nil
}

func (zone *controlledZone) force(ctx context.Context, desired int) error {
	if err := zone.hardware.SetZoneDuty(ctx, zone.cfg.Zone, desired); err != nil {
		return fmt.Errorf("error setting %s zone %d duty to %d: %w", zone.name, zone.cfg.Zone, desired, err)
	}
	zone.duty = desired
	return nil
}

func (zone *controlledZone) setOrFailSafe(ctx context.Context, desired int) (bool, error) {
	changed, err := zone.set(ctx, desired)
	if err == nil {
		return changed, nil
	}
	failureErr := zone.failSafe(ctx, "fan duty write failure")
	return false, errors.Join(err, failureErr)
}

func (zone *controlledZone) failSafe(ctx context.Context, reason string) error {
	zone.log.Error("using failure duty", "event", "failure_duty", "zone", zone.cfg.Zone, "duty", zone.failureDuty, "reason", reason)
	return zone.force(ctx, zone.failureDuty)
}

func (zone *controlledZone) reconcile(ctx context.Context) error {
	actual, err := zone.hardware.GetZoneDuty(ctx, zone.cfg.Zone)
	if err != nil {
		return fmt.Errorf("error reading %s zone %d duty: %w", zone.name, zone.cfg.Zone, err)
	}
	if actual == zone.duty {
		return nil
	}
	zone.log.Warn("fan duty changed externally", "event", "duty_mismatch", "zone", zone.cfg.Zone, "actual", actual, "desired", zone.duty)
	return zone.force(ctx, zone.duty)
}

func (zone *controlledZone) verifyRPM(ctx context.Context, retries int, settleTime time.Duration) error {
	monitor := zone.cfg.RPMMonitor
	if !monitor.Enabled {
		return nil
	}
	for attempt := 0; ; attempt++ {
		rpm, err := zone.hardware.ReadFanRPM(ctx, monitor.Sensor)
		if err != nil {
			return zone.rpmFailure(ctx, fmt.Errorf("error reading %s RPM: %w", monitor.Sensor, err))
		}
		mismatch, reason := fanRPMMismatch(zone.duty, rpm, monitor)
		zone.log.Info("fan RPM checked", "event", "rpm_check", "sensor", monitor.Sensor, "rpm", rpm, "duty", zone.duty, "mismatch", mismatch)
		if !mismatch {
			return nil
		}
		if attempt >= retries {
			return zone.rpmFailure(ctx, errors.New(reason))
		}
		zone.log.Warn("fan RPM mismatch; reapplying duty", "event", "rpm_mismatch", "sensor", monitor.Sensor, "rpm", rpm, "duty", zone.duty, "attempt", attempt+1, "reason", reason)
		if err := zone.force(ctx, zone.duty); err != nil {
			return zone.rpmFailure(ctx, err)
		}
		if err := waitFor(ctx, settleTime); err != nil {
			return err
		}
	}
}

func (zone *controlledZone) rpmFailure(ctx context.Context, cause error) error {
	return errors.Join(cause, zone.failSafe(ctx, "fan RPM verification failure"))
}

type Controller struct {
	cfg        *Config
	hardware   FanHardware
	readCPU    CPUReader
	readDrives DriveReader
	log        *slog.Logger
	cpuZone    *controlledZone
	driveZone  *controlledZone
	driveErr   float64
	oldMode    int
	haveMode   bool
}

func NewController(cfg *Config, hardware FanHardware, cpuReader CPUReader, driveReader DriveReader, logger *slog.Logger) *Controller {
	return &Controller{
		cfg:        cfg,
		hardware:   hardware,
		readCPU:    cpuReader,
		readDrives: driveReader,
		log:        logger,
		cpuZone:    newControlledZone("CPU", cfg.CPU.Fan, cfg.Safety.FailureDuty, hardware, logger),
		driveZone:  newControlledZone("drive", cfg.Drives.Fan, cfg.Safety.FailureDuty, hardware, logger),
	}
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
			controller.runCPUCycle(ctx)
		case <-driveTicker.C:
			if err := controller.updateDrives(ctx); err != nil {
				controller.log.Error("drive update failed", "event", "drive_update_failed", "error", err)
			}
			if err := controller.verifyFanRPMs(ctx); err != nil {
				controller.log.Error("fan RPM verification failed", "event", "rpm_verification_failed", "error", err)
			}
		}
	}
}

func (controller *Controller) runCPUCycle(ctx context.Context) {
	if err := controller.ensureManualMode(ctx); err != nil {
		controller.log.Error("fan mode verification failed", "event", "fan_mode_failed", "error", err)
		controller.useFailureDuties(ctx, "fan mode verification failure")
		return
	}
	if err := controller.updateCPU(ctx); err != nil {
		controller.log.Error("CPU update failed", "event", "cpu_update_failed", "error", err)
	}
	if err := controller.reconcileDuties(ctx); err != nil {
		controller.log.Error("fan duty reconciliation failed", "event", "duty_reconciliation_failed", "error", err)
	}
}

func (controller *Controller) start(ctx context.Context) error {
	mode, err := controller.hardware.GetFanMode(ctx)
	if err != nil {
		return fmt.Errorf("error reading initial fan mode: %w", err)
	}
	controller.oldMode = mode
	controller.haveMode = true
	if mode != controller.cfg.ManualMode {
		if err := controller.hardware.SetFanMode(ctx, controller.cfg.ManualMode); err != nil {
			return fmt.Errorf("error enabling manual fan mode: %w", err)
		}
		if err := waitFor(ctx, controller.cfg.ModeSettleTime.Duration); err != nil {
			return fmt.Errorf("error waiting for manual fan mode: %w", err)
		}
	}

	if err := controller.cpuZone.force(ctx, clampDuty(controller.cfg.InitialDuty, controller.cfg.CPU.Fan)); err != nil {
		return controller.startFailure(ctx, fmt.Errorf("error setting initial CPU duty: %w", err))
	}
	if err := controller.driveZone.force(ctx, clampDuty(controller.cfg.InitialDuty, controller.cfg.Drives.Fan)); err != nil {
		return controller.startFailure(ctx, fmt.Errorf("error setting initial drive duty: %w", err))
	}
	controller.log.Info("fan controller started",
		"event", "startup_completed",
		"old_mode", controller.oldMode,
		"manual_mode", controller.cfg.ManualMode,
		"cpu_duty", controller.cpuZone.duty,
		"drive_duty", controller.driveZone.duty,
		"drive_count", len(controller.cfg.Drives.Devices),
	)
	return nil
}

func (controller *Controller) startFailure(ctx context.Context, cause error) error {
	return errors.Join(
		cause,
		controller.cpuZone.failSafe(ctx, "startup"),
		controller.driveZone.failSafe(ctx, "startup"),
	)
}

func (controller *Controller) restoreMode() {
	if !controller.cfg.RestoreModeOnExit || !controller.haveMode || controller.oldMode == controller.cfg.ManualMode {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), controller.cfg.IPMI.Timeout.Duration)
	defer cancel()
	if err := controller.hardware.SetFanMode(ctx, controller.oldMode); err != nil {
		controller.log.Error("fan mode restoration failed", "event", "fan_mode_restore_failed", "error", err)
		return
	}
	controller.log.Info("fan mode restored", "event", "fan_mode_restored", "mode", controller.oldMode)
}

func (controller *Controller) ensureManualMode(ctx context.Context) error {
	mode, err := controller.hardware.GetFanMode(ctx)
	if err != nil {
		return err
	}
	if mode == controller.cfg.ManualMode {
		return nil
	}
	controller.log.Warn("fan mode changed externally", "event", "fan_mode_changed", "mode", mode)
	if err := controller.hardware.SetFanMode(ctx, controller.cfg.ManualMode); err != nil {
		return fmt.Errorf("error restoring manual fan mode: %w", err)
	}
	if err := waitFor(ctx, controller.cfg.ModeSettleTime.Duration); err != nil {
		return fmt.Errorf("error waiting after manual fan mode restoration: %w", err)
	}
	return errors.Join(
		controller.cpuZone.force(ctx, controller.cpuZone.duty),
		controller.driveZone.force(ctx, controller.driveZone.duty),
	)
}

func (controller *Controller) reconcileDuties(ctx context.Context) error {
	if !controller.cfg.ReadDuty {
		return nil
	}
	cpuErr := controller.cpuZone.reconcile(ctx)
	driveErr := controller.driveZone.reconcile(ctx)
	if cpuErr != nil || driveErr != nil {
		controller.useFailureDuties(ctx, "fan duty reconciliation failure")
	}
	return errors.Join(cpuErr, driveErr)
}

func (controller *Controller) updateCPU(ctx context.Context) error {
	temperature, source, err := controller.readCPU(ctx)
	if err == nil && !validTemperature(temperature) {
		err = fmt.Errorf("CPU reader returned invalid temperature %.1f", temperature)
	}
	if err != nil {
		return errors.Join(err, controller.cpuZone.failSafe(ctx, "CPU sensor failure"))
	}
	duty := calculateCPUDuty(temperature, controller.cfg.CPU)
	changed, err := controller.cpuZone.setOrFailSafe(ctx, duty)
	if err != nil {
		return err
	}
	log := controller.log.Debug
	if changed {
		log = controller.log.Info
	}
	log("CPU fan updated", "event", "cpu_update", "temperature_c", temperature, "source", source, "duty", duty)
	return nil
}

func (controller *Controller) updateDrives(ctx context.Context) error {
	readings := controller.readDrives(ctx, controller.cfg.Drives.Devices)
	result := calculateDriveDuty(controller.driveZone.duty, controller.driveErr, readings, controller.cfg.Drives, controller.cfg.Safety.FailureDuty)
	controller.logDriveIssues(readings)
	if result.Failed > 0 || result.Active == 0 {
		controller.driveErr = 0
	} else {
		controller.driveErr = result.Error
	}
	if _, err := controller.driveZone.setOrFailSafe(ctx, result.Duty); err != nil {
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

func (controller *Controller) useFailureDuties(ctx context.Context, reason string) {
	if err := controller.cpuZone.failSafe(ctx, reason); err != nil {
		controller.log.Error("CPU failure duty failed", "event", "failure_duty_failed", "zone", controller.cfg.CPU.Fan.Zone, "error", err)
	}
	if err := controller.driveZone.failSafe(ctx, reason); err != nil {
		controller.log.Error("drive failure duty failed", "event", "failure_duty_failed", "zone", controller.cfg.Drives.Fan.Zone, "error", err)
	}
}

func (controller *Controller) verifyFanRPMs(ctx context.Context) error {
	if !controller.cfg.CPU.Fan.RPMMonitor.Enabled && !controller.cfg.Drives.Fan.RPMMonitor.Enabled {
		return nil
	}
	if err := waitFor(ctx, controller.cfg.RPMSettleTime.Duration); err != nil {
		return err
	}
	return errors.Join(
		controller.cpuZone.verifyRPM(ctx, controller.cfg.RPMRetries, controller.cfg.RPMSettleTime.Duration),
		controller.driveZone.verifyRPM(ctx, controller.cfg.RPMRetries, controller.cfg.RPMSettleTime.Duration),
	)
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
