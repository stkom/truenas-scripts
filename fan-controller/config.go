package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maximumControlGain = 100.0

type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return errors.New("duration must be a string")
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return err
	}
	d.Duration = parsed
	return nil
}

type CommandConfig struct {
	Path    string   `json:"path"`
	Args    []string `json:"args"`
	Timeout Duration `json:"timeout"`
}

type FanConfig struct {
	Zone       int              `json:"zone"`
	MinDuty    int              `json:"min_duty"`
	MaxDuty    int              `json:"max_duty"`
	RPMMonitor RPMMonitorConfig `json:"rpm_monitor"`
}

type RPMMonitorConfig struct {
	Enabled  bool   `json:"enabled"`
	Sensor   string `json:"sensor"`
	RPMAt30  int    `json:"rpm_at_30"`
	RPMAt100 int    `json:"rpm_at_100"`
}

type CPUConfig struct {
	Fan                 FanConfig `json:"fan"`
	PollInterval        Duration  `json:"poll_interval"`
	ReferenceC          float64   `json:"reference_c"`
	MinimumTemperatureC float64   `json:"minimum_temperature_c"`
	DutyPerDegree       float64   `json:"duty_per_degree"`
	HwmonRoot           string    `json:"hwmon_root"`
	IPMISensor          string    `json:"ipmi_sensor"`
}

type DriveConfig struct {
	Fan                 FanConfig `json:"fan"`
	Devices             []string  `json:"devices"`
	PollInterval        Duration  `json:"poll_interval"`
	SetpointC           float64   `json:"setpoint_c"`
	MinimumTemperatureC float64   `json:"minimum_temperature_c"`
	Kp                  float64   `json:"kp"`
	Kd                  float64   `json:"kd"`
}

type SafetyConfig struct {
	FailureDuty int `json:"failure_duty"`
}

type Config struct {
	IPMI              CommandConfig `json:"ipmi"`
	Smartctl          CommandConfig `json:"smartctl"`
	CPU               CPUConfig     `json:"cpu"`
	Drives            DriveConfig   `json:"drives"`
	Safety            SafetyConfig  `json:"safety"`
	LogPath           string        `json:"log_path"`
	LogMaxSizeMB      int           `json:"log_max_size_mb"`
	LogBackups        int           `json:"log_backups"`
	ManualMode        int           `json:"manual_mode"`
	InitialDuty       int           `json:"initial_duty"`
	RestoreModeOnExit bool          `json:"restore_mode_on_exit"`
	RestoreMode       *int          `json:"restore_mode"`
	RPMSettleTime     Duration      `json:"rpm_settle_time"`
	RPMRetries        int           `json:"rpm_retries"`
	ModeSettleTime    Duration      `json:"mode_settle_time"`
	ReadDuty          bool          `json:"read_duty"`
}

func DefaultConfig() Config {
	return Config{
		IPMI:     CommandConfig{Path: "ipmitool", Timeout: Duration{10 * time.Second}},
		Smartctl: CommandConfig{Path: "smartctl", Timeout: Duration{15 * time.Second}},
		CPU: CPUConfig{
			Fan: FanConfig{
				Zone:    0,
				MinDuty: 30,
				MaxDuty: 100,
				RPMMonitor: RPMMonitorConfig{
					Sensor: "FAN1", RPMAt30: 400, RPMAt100: 1400,
				},
			},
			PollInterval:        Duration{5 * time.Second},
			ReferenceC:          54,
			MinimumTemperatureC: 5,
			DutyPerDegree:       6,
			HwmonRoot:           "/sys/class/hwmon",
			IPMISensor:          "CPU Temp",
		},
		Drives: DriveConfig{
			Fan: FanConfig{
				Zone:    1,
				MinDuty: 20,
				MaxDuty: 100,
				RPMMonitor: RPMMonitorConfig{
					Enabled: true, Sensor: "FANA", RPMAt30: 600, RPMAt100: 1700,
				},
			},
			PollInterval:        Duration{5 * time.Minute},
			SetpointC:           36,
			MinimumTemperatureC: 5,
			Kp:                  4,
			Kd:                  40,
		},
		Safety:            SafetyConfig{FailureDuty: 100},
		LogPath:           "/tmp/fan-controller.log",
		LogMaxSizeMB:      5,
		LogBackups:        2,
		ManualMode:        1,
		InitialDuty:       50,
		RestoreModeOnExit: true,
		RPMSettleTime:     Duration{5 * time.Second},
		RPMRetries:        2,
		ModeSettleTime:    Duration{5 * time.Second},
		ReadDuty:          true,
	}
}

func LoadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	cfg := DefaultConfig()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("error decoding config: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return fmt.Errorf("error reading config: %w", err)
	}
	return errors.New("config contains more than one JSON value")
}

func (cfg *Config) Validate() error {
	if cfg == nil {
		return errors.New("config cannot be nil")
	}
	if err := validateCommand("ipmi", cfg.IPMI); err != nil {
		return err
	}
	if err := validateCommand("smartctl", cfg.Smartctl); err != nil {
		return err
	}
	if err := validateFan("cpu.fan", cfg.CPU.Fan); err != nil {
		return err
	}
	if err := validateFan("drives.fan", cfg.Drives.Fan); err != nil {
		return err
	}
	if cfg.CPU.Fan.Zone == cfg.Drives.Fan.Zone {
		return errors.New("cpu and drive fan zones must differ")
	}
	if cfg.CPU.PollInterval.Duration <= 0 {
		return errors.New("cpu.poll_interval must be positive")
	}
	if err := validateMinimumTemperature("cpu.minimum_temperature_c", cfg.CPU.MinimumTemperatureC); err != nil {
		return err
	}
	if err := validateControlTemperature("cpu.reference_c", cfg.CPU.ReferenceC, cfg.CPU.MinimumTemperatureC); err != nil {
		return err
	}
	if !(cfg.CPU.DutyPerDegree > 0 && cfg.CPU.DutyPerDegree <= maximumControlGain) {
		return fmt.Errorf("cpu.duty_per_degree must be above 0 and at most %.0f", maximumControlGain)
	}
	if strings.TrimSpace(cfg.CPU.HwmonRoot) == "" && strings.TrimSpace(cfg.CPU.IPMISensor) == "" {
		return errors.New("cpu requires hwmon_root or ipmi_sensor")
	}
	if cfg.Drives.PollInterval.Duration <= 0 {
		return errors.New("drives.poll_interval must be positive")
	}
	if err := validateMinimumTemperature("drives.minimum_temperature_c", cfg.Drives.MinimumTemperatureC); err != nil {
		return err
	}
	if err := validateControlTemperature("drives.setpoint_c", cfg.Drives.SetpointC, cfg.Drives.MinimumTemperatureC); err != nil {
		return err
	}
	if !(cfg.Drives.Kp >= 0 && cfg.Drives.Kp <= maximumControlGain) {
		return fmt.Errorf("drives.kp must be between 0 and %.0f", maximumControlGain)
	}
	if !(cfg.Drives.Kd >= 0 && cfg.Drives.Kd <= maximumControlGain) {
		return fmt.Errorf("drives.kd must be between 0 and %.0f", maximumControlGain)
	}
	if len(cfg.Drives.Devices) == 0 {
		return errors.New("drives.devices cannot be empty")
	}
	seen := make(map[string]struct{}, len(cfg.Drives.Devices))
	for index, device := range cfg.Drives.Devices {
		cleaned := filepath.Clean(strings.TrimSpace(device))
		if !filepath.IsAbs(cleaned) || !strings.HasPrefix(cleaned, "/dev/") {
			return fmt.Errorf("drives.devices[%d] must be an absolute /dev path", index)
		}
		if _, exists := seen[cleaned]; exists {
			return fmt.Errorf("duplicate drive device %q", cleaned)
		}
		seen[cleaned] = struct{}{}
		cfg.Drives.Devices[index] = cleaned
	}
	if cfg.Safety.FailureDuty < 1 || cfg.Safety.FailureDuty > 100 {
		return errors.New("safety.failure_duty must be between 1 and 100")
	}
	if cfg.Safety.FailureDuty < cfg.CPU.Fan.MaxDuty || cfg.Safety.FailureDuty < cfg.Drives.Fan.MaxDuty {
		return errors.New("safety.failure_duty cannot be below a configured maximum duty")
	}
	if cfg.LogMaxSizeMB <= 0 || cfg.LogMaxSizeMB > 1024 {
		return errors.New("log_max_size_mb must be between 1 and 1024")
	}
	if cfg.LogBackups < 0 || cfg.LogBackups > 10 {
		return errors.New("log_backups must be between 0 and 10")
	}
	if cfg.InitialDuty < 1 || cfg.InitialDuty > 100 {
		return errors.New("initial_duty must be between 1 and 100")
	}
	if cfg.ManualMode < 0 || cfg.ManualMode > 255 {
		return errors.New("manual_mode must be between 0 and 255")
	}
	if cfg.RestoreMode != nil {
		if *cfg.RestoreMode < 0 || *cfg.RestoreMode > 255 {
			return errors.New("restore_mode must be between 0 and 255")
		}
		if *cfg.RestoreMode == cfg.ManualMode {
			return errors.New("restore_mode must differ from manual_mode")
		}
	}
	if cfg.RPMSettleTime.Duration < 0 {
		return errors.New("rpm_settle_time cannot be negative")
	}
	if cfg.RPMRetries < 0 || cfg.RPMRetries > 10 {
		return errors.New("rpm_retries must be between 0 and 10")
	}
	if cfg.ModeSettleTime.Duration < 0 {
		return errors.New("mode_settle_time cannot be negative")
	}
	return nil
}

func validateCommand(name string, cfg CommandConfig) error {
	if strings.TrimSpace(cfg.Path) == "" {
		return fmt.Errorf("%s.path cannot be empty", name)
	}
	if cfg.Timeout.Duration <= 0 {
		return fmt.Errorf("%s.timeout must be positive", name)
	}
	return nil
}

func validateMinimumTemperature(name string, temperature float64) error {
	if temperature < absoluteMinimumTemperature || temperature >= maximumValidTemperature {
		return fmt.Errorf("%s must be at least %d and below %d", name, absoluteMinimumTemperature, maximumValidTemperature)
	}
	return nil
}

func validateControlTemperature(name string, temperature, minimum float64) error {
	if !validTemperature(temperature, minimum) {
		return fmt.Errorf("%s must be between %g and %d", name, minimum, maximumValidTemperature)
	}
	return nil
}

func validateFan(name string, cfg FanConfig) error {
	if cfg.Zone < 0 || cfg.Zone > 255 {
		return fmt.Errorf("%s.zone must be between 0 and 255", name)
	}
	if cfg.MinDuty < 1 || cfg.MinDuty > 100 {
		return fmt.Errorf("%s.min_duty must be between 1 and 100", name)
	}
	if cfg.MaxDuty < cfg.MinDuty || cfg.MaxDuty > 100 {
		return fmt.Errorf("%s.max_duty must be between min_duty and 100", name)
	}
	if cfg.RPMMonitor.Enabled {
		if strings.TrimSpace(cfg.RPMMonitor.Sensor) == "" {
			return fmt.Errorf("%s.rpm_monitor.sensor cannot be empty when enabled", name)
		}
		if cfg.RPMMonitor.RPMAt30 <= 0 || cfg.RPMMonitor.RPMAt100 <= cfg.RPMMonitor.RPMAt30 {
			return fmt.Errorf("%s RPM calibration must satisfy 0 < rpm_at_30 < rpm_at_100", name)
		}
	}
	return nil
}
