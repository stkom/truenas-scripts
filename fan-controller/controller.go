package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"runtime/debug"
	"sync"
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

// driveSample is the previous drive control error used for the derivative term.
type driveSample struct {
	Error float64
	At    time.Time
	Valid bool
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
			if err == nil && validTemperature(temperature, cfg.MinimumTemperatureC) {
				return temperature, "hwmon", nil
			}
			if err == nil {
				err = fmt.Errorf("hwmon returned invalid CPU temperature %.1f", temperature)
			}
			hwmonErr = err
		}
		if cfg.IPMISensor != "" {
			temperature, err := sensor.ReadTemperature(ctx, cfg.IPMISensor)
			if err == nil && validTemperature(temperature, cfg.MinimumTemperatureC) {
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

// unknownDuty marks a zone whose hardware duty is uncertain after a failed write.
const unknownDuty = -1

// maxUnstableRPMWaits bounds how often an RPM check waits for a changing duty before it is skipped.
const maxUnstableRPMWaits = 6

// zoneFault identifies a condition that overrides a zone's control target with the failure duty.
type zoneFault uint8

const (
	faultSensor zoneFault = 1 << iota
	faultIPMI
	faultRPM
	faultHot
)

// hotDriveHysteresisC is how far the hottest drive must fall below maximum_temperature_c to clear the override.
const hotDriveHysteresisC = 2.0

// controlledZone separates the control algorithm's target from the duty written to hardware.
// Any active fault writes the failure duty without disturbing the target, so control resumes
// from its previous state once every fault clears.
type controlledZone struct {
	name        string
	cfg         FanConfig
	failureDuty int
	hardware    FanHardware
	log         *slog.Logger
	mutation    *sync.Mutex

	mu        sync.Mutex
	target    int
	applied   int
	changedAt time.Time
	faults    zoneFault
}

func newControlledZone(name string, cfg FanConfig, failureDuty int, hardware FanHardware, logger *slog.Logger, mutation *sync.Mutex) *controlledZone {
	return &controlledZone{
		name: name, cfg: cfg, failureDuty: failureDuty, hardware: hardware, log: logger, mutation: mutation,
		target: cfg.MinDuty, applied: unknownDuty,
	}
}

// update sets the control target, clears the given faults and writes the resulting duty if it changed.
func (zone *controlledZone) update(ctx context.Context, target int, clear zoneFault) (bool, error) {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	zone.target = target
	zone.clearFaultsLocked(clear)
	return zone.applyLocked(ctx, false)
}

func (zone *controlledZone) addFault(ctx context.Context, fault zoneFault, reason string) error {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	if zone.faults&fault == 0 {
		zone.log.Error("using failure duty", "event", "failure_duty", "zone", zone.cfg.Zone, "duty", zone.failureDuty, "reason", reason)
	}
	zone.faults |= fault
	_, err := zone.applyLocked(ctx, false)
	return err
}

func (zone *controlledZone) clearFault(ctx context.Context, fault zoneFault) error {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	if zone.faults&fault == 0 {
		return nil
	}
	zone.clearFaultsLocked(fault)
	_, err := zone.applyLocked(ctx, false)
	return err
}

func (zone *controlledZone) clearFaultsLocked(fault zoneFault) {
	if zone.faults&fault == 0 {
		return
	}
	zone.faults &^= fault
	if zone.faults == 0 {
		zone.log.Info("failure duty cleared", "event", "failure_duty_cleared", "zone", zone.cfg.Zone, "target", zone.target)
	}
}

func (zone *controlledZone) outputLocked() int {
	if zone.faults != 0 {
		return zone.failureDuty
	}
	return zone.target
}

// applyLocked writes the zone output when it differs from the last written duty, or always when forced.
// A failed write falls back to the failure duty.
func (zone *controlledZone) applyLocked(ctx context.Context, force bool) (bool, error) {
	desired := zone.outputLocked()
	if !force && desired == zone.applied {
		return false, nil
	}
	err := zone.writeLocked(ctx, desired)
	if err == nil {
		return true, nil
	}
	if desired != zone.failureDuty {
		zone.log.Error("using failure duty", "event", "failure_duty", "zone", zone.cfg.Zone, "duty", zone.failureDuty, "reason", "fan duty write failure")
		err = errors.Join(err, zone.writeLocked(ctx, zone.failureDuty))
	}
	return false, err
}

func (zone *controlledZone) writeLocked(ctx context.Context, duty int) error {
	zone.mutation.Lock()
	defer zone.mutation.Unlock()
	if err := zone.hardware.SetZoneDuty(ctx, zone.cfg.Zone, duty); err != nil {
		zone.applied = unknownDuty
		zone.changedAt = time.Now()
		return fmt.Errorf("error setting %s zone %d duty to %d: %w", zone.name, zone.cfg.Zone, duty, err)
	}
	if duty != zone.applied {
		zone.changedAt = time.Now()
	}
	zone.applied = duty
	return nil
}

// force writes a duty directly, bypassing the target. Used during startup failure and shutdown.
func (zone *controlledZone) force(ctx context.Context, duty int) error {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	return zone.writeLocked(ctx, duty)
}

func (zone *controlledZone) reapply(ctx context.Context) error {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	_, err := zone.applyLocked(ctx, true)
	return err
}

func (zone *controlledZone) currentTarget() int {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	return zone.target
}

func (zone *controlledZone) appliedDuty() int {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	return zone.applied
}

func (zone *controlledZone) appliedState() (int, time.Time) {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	return zone.applied, zone.changedAt
}

func (zone *controlledZone) reconcile(ctx context.Context) error {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	actual, err := zone.hardware.GetZoneDuty(ctx, zone.cfg.Zone)
	if err != nil {
		return fmt.Errorf("error reading %s zone %d duty: %w", zone.name, zone.cfg.Zone, err)
	}
	desired := zone.outputLocked()
	if actual == desired {
		zone.applied = actual
		return nil
	}
	if zone.applied != unknownDuty {
		zone.log.Warn("fan duty changed externally", "event", "duty_mismatch", "zone", zone.cfg.Zone, "actual", actual, "desired", desired)
	}
	_, err = zone.applyLocked(ctx, true)
	return err
}

func (zone *controlledZone) verifyRPM(ctx context.Context, retries int, settleTime time.Duration) error {
	monitor := zone.cfg.RPMMonitor
	if !monitor.Enabled {
		return nil
	}
	// The duty may be changed concurrently by another control loop. Only compare RPM against a duty
	// that has been stable for settleTime and did not change while the sensor was read.
	unstableWaits := 0
	waitForStableDuty := func(wait time.Duration) (bool, error) {
		if unstableWaits >= maxUnstableRPMWaits {
			zone.log.Warn("fan RPM check skipped; duty kept changing", "event", "rpm_check_skipped", "sensor", monitor.Sensor)
			return false, nil
		}
		unstableWaits++
		return true, waitFor(ctx, max(wait, time.Millisecond))
	}
	for attempt := 0; ; {
		duty, changedAt := zone.appliedState()
		if wait := settleTime - time.Since(changedAt); wait > 0 {
			if retry, err := waitForStableDuty(wait); !retry || err != nil {
				return err
			}
			continue
		}
		rpm, err := zone.hardware.ReadFanRPM(ctx, monitor.Sensor)
		if err != nil {
			return zone.rpmFailure(ctx, fmt.Errorf("error reading %s RPM: %w", monitor.Sensor, err))
		}
		if currentDuty, currentChangedAt := zone.appliedState(); currentDuty != duty || !currentChangedAt.Equal(changedAt) {
			if retry, err := waitForStableDuty(settleTime); !retry || err != nil {
				return err
			}
			continue
		}
		mismatch, reason := fanRPMMismatch(duty, rpm, monitor)
		zone.log.Info("fan RPM checked", "event", "rpm_check", "sensor", monitor.Sensor, "rpm", rpm, "duty", duty, "mismatch", mismatch)
		if !mismatch {
			return zone.clearFault(ctx, faultRPM)
		}
		if attempt >= retries {
			return zone.rpmFailure(ctx, errors.New(reason))
		}
		attempt++
		zone.log.Warn("fan RPM mismatch; reapplying duty", "event", "rpm_mismatch", "sensor", monitor.Sensor, "rpm", rpm, "duty", duty, "attempt", attempt, "reason", reason)
		if err := zone.reapply(ctx); err != nil {
			return zone.rpmFailure(ctx, err)
		}
		if err := waitFor(ctx, settleTime); err != nil {
			return err
		}
	}
}

func (zone *controlledZone) rpmFailure(ctx context.Context, cause error) error {
	return errors.Join(cause, zone.addFault(ctx, faultRPM, "fan RPM verification failure"))
}

type Controller struct {
	cfg        *Config
	hardware   FanHardware
	readCPU    CPUReader
	readDrives DriveReader
	log        *slog.Logger
	cpuZone    *controlledZone
	driveZone  *controlledZone
	drivePrev  driveSample
	oldMode    int
	dryRun     bool
	mutation   sync.Mutex
}

// NewController creates a controller. In dry-run mode the hardware is expected to skip writes,
// so the controller also skips checks that compare hardware state with the duties it would have written.
func NewController(cfg *Config, hardware FanHardware, cpuReader CPUReader, driveReader DriveReader, logger *slog.Logger, dryRun bool) *Controller {
	controller := &Controller{
		dryRun:     dryRun,
		cfg:        cfg,
		hardware:   hardware,
		readCPU:    cpuReader,
		readDrives: driveReader,
		log:        logger,
	}
	controller.cpuZone = newControlledZone("CPU", cfg.CPU.Fan, cfg.Safety.FailureDuty, hardware, logger, &controller.mutation)
	controller.driveZone = newControlledZone("drive", cfg.Drives.Fan, cfg.Safety.FailureDuty, hardware, logger, &controller.mutation)
	return controller
}

func (controller *Controller) Run(ctx context.Context, once bool) error {
	if err := controller.start(ctx); err != nil {
		return err
	}
	defer controller.shutdown()

	if err := controller.updateCPU(ctx); err != nil {
		controller.log.Error("CPU update failed", "event", "cpu_update_failed", "error", err)
	}
	if once {
		controller.runDriveCycle(ctx)
		return nil
	}

	runCtx, cancel := context.WithCancel(ctx)
	var driveWorker sync.WaitGroup
	driveFailed := make(chan error, 1)
	driveWorker.Go(func() {
		// A panic here would otherwise kill the process without restoring the fan mode.
		defer func() {
			if recovered := recover(); recovered != nil {
				controller.log.Error("drive loop panicked", "event", "drive_loop_panic", "panic", recovered, "stack", string(debug.Stack()))
				driveFailed <- fmt.Errorf("drive loop panicked: %v", recovered)
			}
		}()
		controller.runDriveLoop(runCtx)
	})
	defer func() {
		cancel()
		driveWorker.Wait()
	}()

	cpuTicker := time.NewTicker(controller.cfg.CPU.PollInterval.Duration)
	defer cpuTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-driveFailed:
			return err
		case <-cpuTicker.C:
			if ctx.Err() != nil {
				return nil
			}
			controller.runCPUCycle(runCtx)
		}
	}
}

func (controller *Controller) runDriveLoop(ctx context.Context) {
	for {
		controller.runDriveCycle(ctx)
		if err := waitFor(ctx, controller.cfg.Drives.PollInterval.Duration); err != nil {
			return
		}
	}
}

func (controller *Controller) runDriveCycle(ctx context.Context) {
	if err := controller.updateDrives(ctx); err != nil && ctx.Err() == nil {
		controller.log.Error("drive update failed", "event", "drive_update_failed", "error", err)
	}
	if ctx.Err() != nil {
		return
	}
	if err := controller.verifyFanRPMs(ctx); err != nil && ctx.Err() == nil {
		controller.log.Error("fan RPM verification failed", "event", "rpm_verification_failed", "error", err)
	}
}

func (controller *Controller) runCPUCycle(ctx context.Context) {
	if controller.dryRun {
		if err := controller.updateCPU(ctx); err != nil {
			controller.log.Error("CPU update failed", "event", "cpu_update_failed", "error", err)
		}
		return
	}
	if err := controller.ensureManualMode(ctx); err != nil {
		controller.log.Error("fan mode verification failed", "event", "fan_mode_failed", "error", err)
		controller.addFaultToBothZones(ctx, faultIPMI, "fan mode verification failure")
		return
	}
	if err := controller.updateCPU(ctx); err != nil {
		controller.log.Error("CPU update failed", "event", "cpu_update_failed", "error", err)
	}
	if err := controller.reconcileDuties(ctx); err != nil {
		controller.log.Error("fan duty reconciliation failed", "event", "duty_reconciliation_failed", "error", err)
		controller.addFaultToBothZones(ctx, faultIPMI, "fan duty reconciliation failure")
		return
	}
	controller.clearFaultFromBothZones(ctx, faultIPMI)
}

func (controller *Controller) start(ctx context.Context) error {
	mode, err := controller.hardware.GetFanMode(ctx)
	if err != nil {
		return fmt.Errorf("error reading initial fan mode: %w", err)
	}
	controller.oldMode = mode
	if controller.cfg.RestoreModeOnExit && controller.cfg.RestoreMode == nil && mode == controller.cfg.ManualMode {
		controller.log.Warn("fan mode is already manual at startup; set restore_mode to restore an automatic mode on exit",
			"event", "restore_mode_unknown", "mode", mode)
	}
	if controller.dryRun {
		controller.log.Info("dry run: fan mode, duty reconciliation and RPM checks are skipped", "event", "dry_run", "mode", mode)
	} else if mode != controller.cfg.ManualMode {
		if err := controller.setFanMode(ctx, controller.cfg.ManualMode); err != nil {
			return fmt.Errorf("error enabling manual fan mode: %w", err)
		}
		if err := waitFor(ctx, controller.cfg.ModeSettleTime.Duration); err != nil {
			return fmt.Errorf("error waiting for manual fan mode: %w", err)
		}
	}

	if _, err := controller.cpuZone.update(ctx, clampDuty(controller.cfg.InitialDuty, controller.cfg.CPU.Fan), 0); err != nil {
		return controller.startFailure(ctx, fmt.Errorf("error setting initial CPU duty: %w", err))
	}
	if _, err := controller.driveZone.update(ctx, clampDuty(controller.cfg.InitialDuty, controller.cfg.Drives.Fan), 0); err != nil {
		return controller.startFailure(ctx, fmt.Errorf("error setting initial drive duty: %w", err))
	}
	controller.log.Info("fan controller started",
		"event", "startup_completed",
		"old_mode", controller.oldMode,
		"manual_mode", controller.cfg.ManualMode,
		"cpu_duty", controller.cpuZone.appliedDuty(),
		"drive_duty", controller.driveZone.appliedDuty(),
		"drive_count", len(controller.cfg.Drives.Devices),
	)
	return nil
}

func (controller *Controller) startFailure(ctx context.Context, cause error) error {
	return errors.Join(
		cause,
		controller.cpuZone.force(ctx, controller.cfg.Safety.FailureDuty),
		controller.driveZone.force(ctx, controller.cfg.Safety.FailureDuty),
	)
}

func (controller *Controller) shutdown() {
	if controller.dryRun {
		controller.log.Info("dry run: no fan changes to undo", "event", "dry_run")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*controller.cfg.IPMI.Timeout.Duration)
	defer cancel()
	restoreMode := controller.oldMode
	if controller.cfg.RestoreMode != nil {
		restoreMode = *controller.cfg.RestoreMode
	}
	if controller.cfg.RestoreModeOnExit && restoreMode != controller.cfg.ManualMode {
		if err := controller.setFanMode(ctx, restoreMode); err == nil {
			controller.log.Info("fan mode restored", "event", "fan_mode_restored", "mode", restoreMode)
			return
		} else {
			controller.log.Error("fan mode restoration failed", "event", "fan_mode_restore_failed", "error", err)
		}
	}
	controller.log.Info("setting failure duties before shutdown", "event", "shutdown_failure_duties", "duty", controller.cfg.Safety.FailureDuty)
	if err := errors.Join(
		controller.cpuZone.force(ctx, controller.cfg.Safety.FailureDuty),
		controller.driveZone.force(ctx, controller.cfg.Safety.FailureDuty),
	); err != nil {
		controller.log.Error("shutdown failure duties failed", "event", "shutdown_failure_duties_failed", "error", err)
	}
}

func (controller *Controller) setFanMode(ctx context.Context, mode int) error {
	controller.mutation.Lock()
	defer controller.mutation.Unlock()
	return controller.hardware.SetFanMode(ctx, mode)
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
	if err := controller.setFanMode(ctx, controller.cfg.ManualMode); err != nil {
		return fmt.Errorf("error restoring manual fan mode: %w", err)
	}
	if err := waitFor(ctx, controller.cfg.ModeSettleTime.Duration); err != nil {
		return fmt.Errorf("error waiting after manual fan mode restoration: %w", err)
	}
	return errors.Join(
		controller.cpuZone.reapply(ctx),
		controller.driveZone.reapply(ctx),
	)
}

func (controller *Controller) reconcileDuties(ctx context.Context) error {
	if !controller.cfg.ReadDuty {
		return nil
	}
	return errors.Join(
		controller.cpuZone.reconcile(ctx),
		controller.driveZone.reconcile(ctx),
	)
}

func (controller *Controller) updateCPU(ctx context.Context) error {
	temperature, source, err := controller.readCPU(ctx)
	if err == nil && !validTemperature(temperature, controller.cfg.CPU.MinimumTemperatureC) {
		err = fmt.Errorf("CPU reader returned invalid temperature %.1f", temperature)
	}
	if err != nil {
		return errors.Join(err, controller.cpuZone.addFault(ctx, faultSensor, "CPU sensor failure"))
	}
	duty := calculateCPUDuty(temperature, controller.cfg.CPU)
	changed, err := controller.cpuZone.update(ctx, duty, faultSensor)
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
	if err := ctx.Err(); err != nil {
		return err
	}
	now := time.Now()
	result := calculateDriveDuty(controller.driveZone.currentTarget(), controller.drivePrev, now, readings, controller.cfg.Drives)
	controller.logDriveIssues(readings)
	if result.Failed > 0 || result.Active == 0 {
		controller.drivePrev = driveSample{}
	} else {
		controller.drivePrev = driveSample{Error: result.Error, At: now, Valid: true}
	}
	if err := controller.applyDriveResult(ctx, result); err != nil {
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
		"target", controller.driveZone.currentTarget(),
		"duty", controller.driveZone.appliedDuty(),
	)
	return nil
}

// applyDriveResult writes the PD target and manages the sensor and hot-drive faults.
func (controller *Controller) applyDriveResult(ctx context.Context, result DriveControlResult) error {
	zone := controller.driveZone
	if result.Failed > 0 {
		return zone.addFault(ctx, faultSensor, "drive sensor failure")
	}
	limit := controller.cfg.Drives.MaximumTemperatureC
	clears := faultSensor
	if limit <= 0 || result.Active == 0 || result.Maximum < limit-hotDriveHysteresisC {
		clears |= faultHot
	}
	// Add the hot fault before updating the target so the zone goes straight to failure duty.
	if limit > 0 && result.Active > 0 && result.Maximum >= limit {
		reason := fmt.Sprintf("hottest drive %.0f°C at or above maximum %.0f°C", result.Maximum, limit)
		if err := zone.addFault(ctx, faultHot, reason); err != nil {
			return err
		}
	}
	_, err := zone.update(ctx, result.Duty, clears)
	return err
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

func (controller *Controller) addFaultToBothZones(ctx context.Context, fault zoneFault, reason string) {
	for _, zone := range []*controlledZone{controller.cpuZone, controller.driveZone} {
		if err := zone.addFault(ctx, fault, reason); err != nil {
			controller.log.Error("failure duty failed", "event", "failure_duty_failed", "zone", zone.cfg.Zone, "error", err)
		}
	}
}

func (controller *Controller) clearFaultFromBothZones(ctx context.Context, fault zoneFault) {
	for _, zone := range []*controlledZone{controller.cpuZone, controller.driveZone} {
		if err := zone.clearFault(ctx, fault); err != nil {
			controller.log.Error("fan duty restore failed", "event", "duty_restore_failed", "zone", zone.cfg.Zone, "error", err)
		}
	}
}

func (controller *Controller) verifyFanRPMs(ctx context.Context) error {
	if controller.dryRun {
		return nil
	}
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
	if duty < 0 {
		return true, "fan duty is unknown"
	}
	if math.IsNaN(rpm) {
		return true, "RPM is not a number"
	}
	expected := float64(monitor.RPMAt30) * float64(duty) / 30
	if duty > 30 {
		expected = float64(monitor.RPMAt30) + float64(duty-30)*float64(monitor.RPMAt100-monitor.RPMAt30)/70
	}
	minimum := 0.5 * expected
	if duty >= 95 {
		minimum = 0.8 * float64(monitor.RPMAt100)
	}
	if rpm < minimum {
		return true, fmt.Sprintf("RPM %.0f below expected minimum %.0f", rpm, minimum)
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
	return clampFloatDuty(math.Ceil(demand), cfg.Fan)
}

// calculateDriveDuty applies the legacy spinpid2 PD step to the current target duty.
// The derivative uses the measured time since the previous sample and is zero without one.
// When any reading failed, Duty is left at the current target; the caller applies the failure duty.
func calculateDriveDuty(current int, previous driveSample, now time.Time, readings []DriveReading, cfg DriveConfig) DriveControlResult {
	result := DriveControlResult{Duty: current}
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
	if minutes := now.Sub(previous.At).Minutes(); previous.Valid && minutes > 0 {
		result.Derivative = cfg.Kd * (result.Error - previous.Error) / minutes
	}
	demand := float64(current) + result.Proportional + result.Derivative
	result.Duty = clampFloatDuty(math.Round(demand), cfg.Fan)
	return result
}

func clampFloatDuty(value float64, cfg FanConfig) int {
	if math.IsNaN(value) {
		return cfg.MaxDuty
	}
	if value <= float64(cfg.MinDuty) {
		return cfg.MinDuty
	}
	if value >= float64(cfg.MaxDuty) {
		return cfg.MaxDuty
	}
	return int(value)
}

func clampDuty(value int, cfg FanConfig) int {
	return max(cfg.MinDuty, min(cfg.MaxDuty, value))
}
