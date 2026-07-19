package main

import "testing"

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
		{name: "middle duty unchecked", duty: 50, rpm: 0, want: false},
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
