package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type dutyWrite struct {
	zone int
	duty int
}

type fakeFanHardware struct {
	mu            sync.Mutex
	mode          int
	duties        map[int]int
	rpms          map[string]float64
	modeWriteErr  error
	modeReadErr   error
	dutyReadErr   map[int]error
	failingDuties map[int]bool
	modeWrites    []int
	dutyWrites    []dutyWrite
}

func newFakeFanHardware() *fakeFanHardware {
	return &fakeFanHardware{
		duties:        map[int]int{},
		rpms:          map[string]float64{},
		dutyReadErr:   map[int]error{},
		failingDuties: map[int]bool{},
	}
}

func (hardware *fakeFanHardware) GetFanMode(context.Context) (int, error) {
	hardware.mu.Lock()
	defer hardware.mu.Unlock()
	return hardware.mode, hardware.modeReadErr
}

func (hardware *fakeFanHardware) SetFanMode(_ context.Context, mode int) error {
	hardware.mu.Lock()
	defer hardware.mu.Unlock()
	hardware.modeWrites = append(hardware.modeWrites, mode)
	if hardware.modeWriteErr != nil {
		return hardware.modeWriteErr
	}
	hardware.mode = mode
	return nil
}

func (hardware *fakeFanHardware) GetZoneDuty(_ context.Context, zone int) (int, error) {
	hardware.mu.Lock()
	defer hardware.mu.Unlock()
	return hardware.duties[zone], hardware.dutyReadErr[zone]
}

func (hardware *fakeFanHardware) SetZoneDuty(_ context.Context, zone, duty int) error {
	hardware.mu.Lock()
	defer hardware.mu.Unlock()
	hardware.dutyWrites = append(hardware.dutyWrites, dutyWrite{zone: zone, duty: duty})
	if hardware.failingDuties[duty] {
		return errors.New("duty write failed")
	}
	hardware.duties[zone] = duty
	return nil
}

func (hardware *fakeFanHardware) ReadFanRPM(_ context.Context, sensor string) (float64, error) {
	hardware.mu.Lock()
	defer hardware.mu.Unlock()
	return hardware.rpms[sensor], nil
}

func newTestZone(hardware FanHardware, cfg FanConfig) *controlledZone {
	return newControlledZone("test", cfg, 100, hardware, slog.New(slog.NewTextHandler(io.Discard, nil)), &sync.Mutex{})
}

func setZoneState(zone *controlledZone, target, applied int) {
	zone.mu.Lock()
	defer zone.mu.Unlock()
	zone.target = target
	zone.applied = applied
}

func newTestController(cfg *Config, hardware FanHardware, cpuReader CPUReader, driveReader DriveReader) *Controller {
	return NewController(cfg, hardware, cpuReader, driveReader, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestCalculateCPUDuty(t *testing.T) {
	cfg := DefaultConfig().CPU
	tests := []struct {
		name        string
		temperature float64
		want        int
	}{
		{name: "below reference", temperature: 40, want: 30},
		{name: "one degree high", temperature: 55, want: 36},
		{name: "fraction rounds safely up", temperature: 55.1, want: 37},
		{name: "maximum", temperature: 100, want: 100},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := calculateCPUDuty(test.temperature, cfg); got != test.want {
				t.Errorf("calculateCPUDuty(%v) = %d, want %d", test.temperature, got, test.want)
			}
		})
	}
}

func TestDutyCalculationsSaturateBeforeIntegerConversion(t *testing.T) {
	cpuCfg := DefaultConfig().CPU
	cpuCfg.DutyPerDegree = math.MaxFloat64
	if got := calculateCPUDuty(cpuCfg.ReferenceC+1, cpuCfg); got != cpuCfg.Fan.MaxDuty {
		t.Errorf("CPU duty = %d, want maximum %d", got, cpuCfg.Fan.MaxDuty)
	}

	driveCfg := DefaultConfig().Drives
	driveCfg.Kp = math.MaxFloat64
	driveCfg.Kd = 0
	result := calculateDriveDuty(50, 0, []DriveReading{{State: DriveActive, Temperature: driveCfg.SetpointC + 1}}, driveCfg)
	if result.Duty != driveCfg.Fan.MaxDuty {
		t.Errorf("drive duty = %d, want maximum %d", result.Duty, driveCfg.Fan.MaxDuty)
	}

	if got := clampFloatDuty(math.NaN(), cpuCfg.Fan); got != cpuCfg.Fan.MaxDuty {
		t.Errorf("NaN duty = %d, want safe maximum %d", got, cpuCfg.Fan.MaxDuty)
	}
}

func TestCalculateDriveDutyMatchesLegacyPDMath(t *testing.T) {
	cfg := DefaultConfig().Drives
	readings := []DriveReading{
		{Device: "/dev/sda", State: DriveActive, Temperature: 36},
		{Device: "/dev/sdb", State: DriveActive, Temperature: 38},
	}

	result := calculateDriveDuty(50, 0, readings, cfg)
	if result.Mean != 37 || result.Maximum != 38 {
		t.Errorf("temperatures mean/max = %v/%v, want 37/38", result.Mean, result.Maximum)
	}
	if result.Proportional != 4 || result.Derivative != 8 {
		t.Errorf("P/D = %v/%v, want 4/8", result.Proportional, result.Derivative)
	}
	if result.Duty != 62 {
		t.Errorf("Duty = %d, want 62", result.Duty)
	}
}

func TestCalculateDriveDutyHandlesStandbyAndFailure(t *testing.T) {
	cfg := DefaultConfig().Drives

	standby := calculateDriveDuty(50, 2, []DriveReading{{State: DriveStandby}}, cfg)
	if standby.Duty != cfg.Fan.MinDuty {
		t.Errorf("standby duty = %d, want %d", standby.Duty, cfg.Fan.MinDuty)
	}

	failed := calculateDriveDuty(50, 2, []DriveReading{{State: DriveFailed}}, cfg)
	if failed.Failed != 1 || failed.Duty != 50 {
		t.Errorf("failed result = %+v, want one failure and unchanged target 50", failed)
	}
}

func TestFanRPMMismatch(t *testing.T) {
	monitor := RPMMonitorConfig{RPMAt30: 600, RPMAt100: 1700}
	tests := []struct {
		name string
		duty int
		rpm  float64
		want bool
	}{
		{name: "high duty too slow", duty: 100, rpm: 1200, want: true},
		{name: "high duty valid", duty: 100, rpm: 1400, want: false},
		{name: "low duty too fast", duty: 20, rpm: 800, want: true},
		{name: "low duty valid", duty: 20, rpm: 700, want: false},
		{name: "stopped at low duty", duty: 20, rpm: 0, want: true},
		{name: "stopped at middle duty", duty: 50, rpm: 0, want: true},
		{name: "middle duty valid", duty: 50, rpm: 700, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, _ := fanRPMMismatch(test.duty, test.rpm, monitor)
			if got != test.want {
				t.Errorf("fanRPMMismatch(%d, %v) = %t, want %t", test.duty, test.rpm, got, test.want)
			}
		})
	}
}

func TestControllerRestoresModeAndReappliesBothZones(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives.Devices = []string{"/dev/sda"}
	cfg.ModeSettleTime.Duration = 0
	cfg.ReadDuty = false
	hardware := newFakeFanHardware()
	hardware.mode = 0
	hardware.duties[0] = 100
	hardware.duties[1] = 100
	controller := newTestController(&cfg, hardware, nil, nil)
	setZoneState(controller.cpuZone, 30, 30)
	setZoneState(controller.driveZone, 52, 52)

	if err := controller.ensureManualMode(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hardware.mode != cfg.ManualMode || hardware.duties[0] != 30 || hardware.duties[1] != 52 {
		t.Errorf("mode/duties = %d/%d/%d, want %d/30/52", hardware.mode, hardware.duties[0], hardware.duties[1], cfg.ManualMode)
	}
	if len(hardware.dutyWrites) != 2 {
		t.Errorf("duty writes = %v, want forced writes for both zones", hardware.dutyWrites)
	}
}

func TestControllerModeFailureUsesBothFailureDuties(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives.Devices = []string{"/dev/sda"}
	hardware := newFakeFanHardware()
	hardware.mode = 0
	hardware.modeWriteErr = errors.New("mode write failed")
	controller := newTestController(&cfg, hardware,
		func(context.Context) (float64, string, error) { return 40, "test", nil },
		func(context.Context, []string) []DriveReading { return nil },
	)
	setZoneState(controller.cpuZone, 30, 30)
	setZoneState(controller.driveZone, 50, 50)

	controller.runCPUCycle(context.Background())
	if controller.cpuZone.appliedDuty() != 100 || controller.driveZone.appliedDuty() != 100 {
		t.Errorf("applied duties = %d/%d, want 100/100", controller.cpuZone.appliedDuty(), controller.driveZone.appliedDuty())
	}
	if hardware.duties[0] != 100 || hardware.duties[1] != 100 {
		t.Errorf("hardware duties = %d/%d, want 100/100", hardware.duties[0], hardware.duties[1])
	}
}

func TestControlledZoneWriteFailureUsesFailureDuty(t *testing.T) {
	hardware := newFakeFanHardware()
	hardware.failingDuties[60] = true
	zone := newTestZone(hardware, FanConfig{Zone: 1})
	setZoneState(zone, 40, 40)

	if _, err := zone.update(context.Background(), 60, 0); err == nil {
		t.Fatal("update() error is nil")
	}
	if zone.appliedDuty() != 100 || hardware.duties[1] != 100 {
		t.Errorf("failure duty applied/hardware = %d/%d, want 100/100", zone.appliedDuty(), hardware.duties[1])
	}

	hardware.failingDuties[100] = true
	if _, err := zone.update(context.Background(), 60, 0); err == nil {
		t.Fatal("update() error is nil")
	}
	if zone.appliedDuty() != unknownDuty {
		t.Errorf("applied duty = %d after failed writes, want unknown", zone.appliedDuty())
	}
}

func TestControlledZoneFaultPreservesTarget(t *testing.T) {
	hardware := newFakeFanHardware()
	zone := newTestZone(hardware, FanConfig{Zone: 1})
	ctx := context.Background()
	if _, err := zone.update(ctx, 45, 0); err != nil {
		t.Fatal(err)
	}

	if err := zone.addFault(ctx, faultIPMI, "test"); err != nil {
		t.Fatal(err)
	}
	if err := zone.addFault(ctx, faultSensor, "test"); err != nil {
		t.Fatal(err)
	}
	if hardware.duties[1] != 100 || zone.currentTarget() != 45 {
		t.Errorf("hardware/target = %d/%d during faults, want 100/45", hardware.duties[1], zone.currentTarget())
	}

	if err := zone.clearFault(ctx, faultIPMI); err != nil {
		t.Fatal(err)
	}
	if hardware.duties[1] != 100 {
		t.Errorf("hardware = %d with sensor fault still active, want 100", hardware.duties[1])
	}
	if _, err := zone.update(ctx, 47, faultSensor); err != nil {
		t.Fatal(err)
	}
	if hardware.duties[1] != 47 {
		t.Errorf("hardware = %d after all faults cleared, want 47", hardware.duties[1])
	}
}

func TestControllerCPUSensorFailureUsesFailureDuty(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives.Devices = []string{"/dev/sda"}
	hardware := newFakeFanHardware()
	controller := newTestController(&cfg, hardware,
		func(context.Context) (float64, string, error) { return 0, "", errors.New("sensor failed") },
		nil,
	)
	setZoneState(controller.cpuZone, 30, 30)

	if err := controller.updateCPU(context.Background()); err == nil {
		t.Fatal("updateCPU() error is nil")
	}
	if controller.cpuZone.appliedDuty() != 100 || hardware.duties[0] != 100 {
		t.Errorf("CPU failure duty applied/hardware = %d/%d, want 100/100", controller.cpuZone.appliedDuty(), hardware.duties[0])
	}
}

func TestControllerReconcileFailureUsesFailureDutiesUntilRecovery(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives.Devices = []string{"/dev/sda"}
	hardware := newFakeFanHardware()
	hardware.duties[0] = 30
	hardware.duties[1] = 50
	hardware.dutyReadErr[0] = errors.New("read failed")
	controller := newTestController(&cfg, hardware, nil, nil)
	hardware.mode = cfg.ManualMode
	controller.readCPU = func(context.Context) (float64, string, error) { return 40, "test", nil }
	setZoneState(controller.cpuZone, 30, 30)
	setZoneState(controller.driveZone, 50, 50)

	controller.runCPUCycle(context.Background())
	if controller.cpuZone.appliedDuty() != 100 || controller.driveZone.appliedDuty() != 100 {
		t.Errorf("failure duties = %d/%d, want 100/100", controller.cpuZone.appliedDuty(), controller.driveZone.appliedDuty())
	}
	if controller.driveZone.currentTarget() != 50 {
		t.Errorf("drive target = %d, want preserved 50", controller.driveZone.currentTarget())
	}

	hardware.dutyReadErr[0] = nil
	controller.runCPUCycle(context.Background())
	if hardware.duties[0] != 30 || hardware.duties[1] != 50 {
		t.Errorf("duties after recovery = %d/%d, want 30/50", hardware.duties[0], hardware.duties[1])
	}
}

func TestControlledZoneRPMMismatchUsesFailureDuty(t *testing.T) {
	hardware := newFakeFanHardware()
	hardware.rpms["FANA"] = 1000
	zone := newTestZone(hardware, FanConfig{
		Zone: 1,
		RPMMonitor: RPMMonitorConfig{
			Enabled: true, Sensor: "FANA", RPMAt30: 600, RPMAt100: 1700,
		},
	})
	setZoneState(zone, 20, 20)

	if err := zone.verifyRPM(context.Background(), 0, 0); err == nil {
		t.Fatal("verifyRPM() error is nil")
	}
	if zone.appliedDuty() != 100 || hardware.duties[1] != 100 {
		t.Errorf("RPM failure duty applied/hardware = %d/%d, want 100/100", zone.appliedDuty(), hardware.duties[1])
	}

	hardware.rpms["FANA"] = 1600
	if err := zone.verifyRPM(context.Background(), 0, 0); err != nil {
		t.Fatal(err)
	}
	if hardware.duties[1] != 20 {
		t.Errorf("duty after RPM recovery = %d, want target 20", hardware.duties[1])
	}
}

func TestControllerRejectsImplausiblyLowCPUTemperature(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives.Devices = []string{"/dev/sda"}
	hardware := newFakeFanHardware()
	controller := newTestController(&cfg, hardware,
		func(context.Context) (float64, string, error) { return 0, "test", nil },
		nil,
	)
	setZoneState(controller.cpuZone, 30, 30)

	if err := controller.updateCPU(context.Background()); err == nil {
		t.Fatal("updateCPU() error is nil")
	}
	if controller.cpuZone.appliedDuty() != cfg.Safety.FailureDuty {
		t.Errorf("CPU duty = %d, want %d", controller.cpuZone.appliedDuty(), cfg.Safety.FailureDuty)
	}
}

func TestControllerShutdownUsesFailureDutiesWhenStartupModeWasManual(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives.Devices = []string{"/dev/sda"}
	cfg.ModeSettleTime.Duration = 0
	cfg.RPMSettleTime.Duration = 0
	cfg.CPU.Fan.RPMMonitor.Enabled = false
	cfg.Drives.Fan.RPMMonitor.Enabled = false
	hardware := newFakeFanHardware()
	hardware.mode = cfg.ManualMode
	controller := newTestController(&cfg, hardware,
		func(context.Context) (float64, string, error) { return 40, "test", nil },
		func(context.Context, []string) []DriveReading { return []DriveReading{{State: DriveStandby}} },
	)

	if err := controller.Run(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if hardware.duties[cfg.CPU.Fan.Zone] != cfg.Safety.FailureDuty || hardware.duties[cfg.Drives.Fan.Zone] != cfg.Safety.FailureDuty {
		t.Errorf("shutdown duties = %d/%d, want %d/%d",
			hardware.duties[cfg.CPU.Fan.Zone], hardware.duties[cfg.Drives.Fan.Zone],
			cfg.Safety.FailureDuty, cfg.Safety.FailureDuty)
	}
}

func TestControllerShutdownRestoresAutomaticMode(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives.Devices = []string{"/dev/sda"}
	cfg.ModeSettleTime.Duration = 0
	cfg.RPMSettleTime.Duration = 0
	cfg.CPU.Fan.RPMMonitor.Enabled = false
	cfg.Drives.Fan.RPMMonitor.Enabled = false
	hardware := newFakeFanHardware()
	hardware.mode = 0
	controller := newTestController(&cfg, hardware,
		func(context.Context) (float64, string, error) { return 40, "test", nil },
		func(context.Context, []string) []DriveReading { return []DriveReading{{State: DriveStandby}} },
	)

	if err := controller.Run(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if hardware.mode != 0 {
		t.Errorf("shutdown mode = %d, want startup mode 0", hardware.mode)
	}
	if hardware.duties[cfg.CPU.Fan.Zone] == cfg.Safety.FailureDuty || hardware.duties[cfg.Drives.Fan.Zone] == cfg.Safety.FailureDuty {
		t.Errorf("duties unexpectedly set to failure duty: %v", hardware.duties)
	}
}

func TestControllerShutdownFallsBackWhenModeRestorationFails(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives.Devices = []string{"/dev/sda"}
	hardware := newFakeFanHardware()
	hardware.mode = cfg.ManualMode
	hardware.modeWriteErr = errors.New("mode write failed")
	controller := newTestController(&cfg, hardware, nil, nil)
	controller.oldMode = 0

	controller.shutdown()
	if hardware.duties[cfg.CPU.Fan.Zone] != cfg.Safety.FailureDuty || hardware.duties[cfg.Drives.Fan.Zone] != cfg.Safety.FailureDuty {
		t.Errorf("fallback duties = %d/%d, want %d/%d",
			hardware.duties[cfg.CPU.Fan.Zone], hardware.duties[cfg.Drives.Fan.Zone],
			cfg.Safety.FailureDuty, cfg.Safety.FailureDuty)
	}
}

func TestSlowDriveCycleDoesNotBlockCPUCycles(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives.Devices = []string{"/dev/sda"}
	cfg.CPU.PollInterval.Duration = 5 * time.Millisecond
	cfg.Drives.PollInterval.Duration = time.Hour
	cfg.ModeSettleTime.Duration = 0
	cfg.ReadDuty = false
	cfg.CPU.Fan.RPMMonitor.Enabled = false
	cfg.Drives.Fan.RPMMonitor.Enabled = false
	hardware := newFakeFanHardware()
	hardware.mode = cfg.ManualMode
	driveStarted := make(chan struct{})
	var cpuReads atomic.Int32
	controller := newTestController(&cfg, hardware,
		func(context.Context) (float64, string, error) {
			cpuReads.Add(1)
			return 40, "test", nil
		},
		func(ctx context.Context, _ []string) []DriveReading {
			close(driveStarted)
			<-ctx.Done()
			return nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- controller.Run(ctx, false) }()
	select {
	case <-driveStarted:
	case <-time.After(time.Second):
		t.Fatal("drive cycle did not start")
	}
	deadline := time.After(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for cpuReads.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("CPU cycle was blocked by drive reading")
		case <-ticker.C:
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("controller did not stop")
	}
}

func TestControllerDriveFailureKeepsPDTarget(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives.Devices = []string{"/dev/sda"}
	cfg.Drives.Kd = 0
	hardware := newFakeFanHardware()
	readings := []DriveReading{{State: DriveFailed, Err: errors.New("timeout")}}
	controller := newTestController(&cfg, hardware, nil,
		func(context.Context, []string) []DriveReading { return readings },
	)
	setZoneState(controller.driveZone, 50, 50)
	ctx := context.Background()

	if err := controller.updateDrives(ctx); err != nil {
		t.Fatal(err)
	}
	if hardware.duties[1] != 100 || controller.driveZone.currentTarget() != 50 {
		t.Errorf("hardware/target = %d/%d after failure, want 100/50", hardware.duties[1], controller.driveZone.currentTarget())
	}

	readings = []DriveReading{{State: DriveActive, Temperature: cfg.Drives.SetpointC - 1}}
	if err := controller.updateDrives(ctx); err != nil {
		t.Fatal(err)
	}
	if want := 50 - int(cfg.Drives.Kp); hardware.duties[1] != want {
		t.Errorf("hardware = %d after recovery, want %d", hardware.duties[1], want)
	}
}
