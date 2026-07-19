package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
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

func TestCalculateDriveDutyMatchesLegacyPDMath(t *testing.T) {
	cfg := DefaultConfig().Drives
	readings := []DriveReading{
		{Device: "/dev/sda", State: DriveActive, Temperature: 36},
		{Device: "/dev/sdb", State: DriveActive, Temperature: 38},
	}

	result := calculateDriveDuty(50, 0, readings, cfg, 100)
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

	standby := calculateDriveDuty(50, 2, []DriveReading{{State: DriveStandby}}, cfg, 100)
	if standby.Duty != cfg.Fan.MinDuty {
		t.Errorf("standby duty = %d, want %d", standby.Duty, cfg.Fan.MinDuty)
	}

	failed := calculateDriveDuty(50, 2, []DriveReading{{State: DriveFailed}}, cfg, 100)
	if failed.Duty != 100 {
		t.Errorf("failure duty = %d, want 100", failed.Duty)
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
	controller.cpuZone.duty = 30
	controller.driveZone.duty = 52

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
	controller.cpuZone.duty = 30
	controller.driveZone.duty = 50

	controller.runCPUCycle(context.Background())
	if controller.cpuZone.duty != 100 || controller.driveZone.duty != 100 {
		t.Errorf("cached duties = %d/%d, want 100/100", controller.cpuZone.duty, controller.driveZone.duty)
	}
	if hardware.duties[0] != 100 || hardware.duties[1] != 100 {
		t.Errorf("hardware duties = %d/%d, want 100/100", hardware.duties[0], hardware.duties[1])
	}
}

func TestControlledZoneCacheChangesOnlyAfterSuccessfulWrite(t *testing.T) {
	hardware := newFakeFanHardware()
	hardware.failingDuties[60] = true
	zone := newControlledZone("test", FanConfig{Zone: 1}, 100, hardware, slog.New(slog.NewTextHandler(io.Discard, nil)), &sync.Mutex{})
	zone.duty = 40

	if _, err := zone.set(context.Background(), 60); err == nil {
		t.Fatal("set() error is nil")
	}
	if zone.duty != 40 {
		t.Errorf("duty = %d after failed write, want 40", zone.duty)
	}
	if _, err := zone.setOrFailSafe(context.Background(), 60); err == nil {
		t.Fatal("setOrFailSafe() error is nil")
	}
	if zone.duty != 100 || hardware.duties[1] != 100 {
		t.Errorf("failure duty cache/hardware = %d/%d, want 100/100", zone.duty, hardware.duties[1])
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
	controller.cpuZone.duty = 30

	if err := controller.updateCPU(context.Background()); err == nil {
		t.Fatal("updateCPU() error is nil")
	}
	if controller.cpuZone.duty != 100 || hardware.duties[0] != 100 {
		t.Errorf("CPU failure duty cache/hardware = %d/%d, want 100/100", controller.cpuZone.duty, hardware.duties[0])
	}
}

func TestControllerReconcileFailureUsesBothFailureDuties(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives.Devices = []string{"/dev/sda"}
	hardware := newFakeFanHardware()
	hardware.duties[0] = 30
	hardware.duties[1] = 50
	hardware.dutyReadErr[0] = errors.New("read failed")
	controller := newTestController(&cfg, hardware, nil, nil)
	controller.cpuZone.duty = 30
	controller.driveZone.duty = 50

	if err := controller.reconcileDuties(context.Background()); err == nil {
		t.Fatal("reconcileDuties() error is nil")
	}
	if controller.cpuZone.duty != 100 || controller.driveZone.duty != 100 {
		t.Errorf("failure duties = %d/%d, want 100/100", controller.cpuZone.duty, controller.driveZone.duty)
	}
}

func TestControlledZoneRPMMismatchUsesFailureDuty(t *testing.T) {
	hardware := newFakeFanHardware()
	hardware.rpms["FANA"] = 1000
	zone := newControlledZone("drive", FanConfig{
		Zone: 1,
		RPMMonitor: RPMMonitorConfig{
			Enabled: true, Sensor: "FANA", RPMAt30: 600, RPMAt100: 1700,
		},
	}, 100, hardware, slog.New(slog.NewTextHandler(io.Discard, nil)), &sync.Mutex{})
	zone.duty = 20

	if err := zone.verifyRPM(context.Background(), 0, 0); err == nil {
		t.Fatal("verifyRPM() error is nil")
	}
	if zone.duty != 100 || hardware.duties[1] != 100 {
		t.Errorf("RPM failure duty cache/hardware = %d/%d, want 100/100", zone.duty, hardware.duties[1])
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
	controller.cpuZone.duty = 30

	if err := controller.updateCPU(context.Background()); err == nil {
		t.Fatal("updateCPU() error is nil")
	}
	if controller.cpuZone.currentDuty() != cfg.Safety.FailureDuty {
		t.Errorf("CPU duty = %d, want %d", controller.cpuZone.currentDuty(), cfg.Safety.FailureDuty)
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
