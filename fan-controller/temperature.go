package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

type DriveState string

const (
	DriveActive  DriveState = "active"
	DriveStandby DriveState = "standby"
	DriveFailed  DriveState = "failed"
)

type DriveReading struct {
	Device      string
	State       DriveState
	Temperature float64
	Warning     string
	Err         error
}

type smartctlOutput struct {
	PowerMode *struct {
		Name string `json:"name"`
	} `json:"power_mode"`
	Smartctl struct {
		ExitStatus int      `json:"exit_status"`
		Output     []string `json:"output"`
	} `json:"smartctl"`
	Temperature struct {
		Current *float64 `json:"current"`
	} `json:"temperature"`
}

type SmartctlClient struct {
	cfg                CommandConfig
	minimumTemperature float64
	command            ConfiguredCommand
}

func NewSmartctlClient(cfg CommandConfig, minimumTemperature float64, runner CommandRunner) *SmartctlClient {
	return &SmartctlClient{cfg: cfg, minimumTemperature: minimumTemperature, command: NewConfiguredCommand(cfg, runner)}
}

func (client *SmartctlClient) ReadTemperature(ctx context.Context, device string) DriveReading {
	result, err := client.command.Run(ctx, "-a", "--json=o", "-n", "standby", device)
	if err != nil {
		return failedDrive(device, err)
	}

	var output smartctlOutput
	if err := json.Unmarshal(result.Stdout, &output); err != nil {
		if result.ExitCode != 0 {
			return failedDrive(device, commandExitError(client.cfg.Path, result))
		}
		return failedDrive(device, fmt.Errorf("error decoding smartctl JSON: %w", err))
	}
	exitStatus := output.Smartctl.ExitStatus
	if exitStatus == 0 && result.ExitCode != 0 {
		exitStatus = result.ExitCode
	}
	if smartctlReportsStandby(output) {
		return DriveReading{Device: device, State: DriveStandby}
	}
	if exitStatus&0x03 != 0 {
		return failedDrive(device, fmt.Errorf("smartctl could not inspect device (status %d)", exitStatus))
	}
	if output.Temperature.Current == nil {
		return failedDrive(device, errors.New("smartctl returned no temperature"))
	}
	temperature := *output.Temperature.Current
	if !validTemperature(temperature, client.minimumTemperature) {
		return failedDrive(device, fmt.Errorf("smartctl returned invalid temperature %.1f", temperature))
	}
	reading := DriveReading{Device: device, State: DriveActive, Temperature: temperature}
	if exitStatus&0xfc != 0 {
		reading.Warning = fmt.Sprintf("SMART health status bits set: %d", exitStatus)
	}
	return reading
}

func failedDrive(device string, err error) DriveReading {
	return DriveReading{Device: device, State: DriveFailed, Err: err}
}

func smartctlReportsStandby(output smartctlOutput) bool {
	if output.PowerMode != nil && isStandbyMode(output.PowerMode.Name) {
		return true
	}
	for _, line := range output.Smartctl.Output {
		line = strings.ToLower(strings.TrimSpace(line))
		if (strings.Contains(line, "device is in") || strings.Contains(line, "power mode")) &&
			(strings.Contains(line, "standby") || strings.Contains(line, "sleep")) {
			return true
		}
	}
	return false
}

func isStandbyMode(powerMode string) bool {
	mode := strings.ToLower(strings.TrimSpace(powerMode))
	return mode == "standby" || mode == "sleep"
}

func (client *SmartctlClient) ReadTemperatures(ctx context.Context, devices []string) []DriveReading {
	readings := make([]DriveReading, len(devices))
	var wg sync.WaitGroup
	for index, device := range devices {
		wg.Go(func() {
			readings[index] = client.ReadTemperature(ctx, device)
		})
	}
	wg.Wait()
	return readings
}

func ReadHwmonCPUTemperature(root string) (float64, error) {
	hwmons, err := filepath.Glob(filepath.Join(root, "hwmon*"))
	if err != nil {
		return 0, err
	}
	var temperatures []float64
	for _, hwmon := range hwmons {
		name, err := readTrimmed(filepath.Join(hwmon, "name"))
		if err != nil || !supportedCPUHwmon(name) {
			continue
		}
		labels, err := filepath.Glob(filepath.Join(hwmon, "temp*_label"))
		if err != nil {
			continue
		}
		for _, labelPath := range labels {
			label, err := readTrimmed(labelPath)
			if err != nil || !isPackageTemperature(label) {
				continue
			}
			inputPath := strings.TrimSuffix(labelPath, "_label") + "_input"
			raw, err := readTrimmed(inputPath)
			if err != nil {
				continue
			}
			millidegrees, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				continue
			}
			temperature := millidegrees / 1000
			if validTemperature(temperature, absoluteMinimumTemperature) {
				temperatures = append(temperatures, temperature)
			}
		}
	}
	if len(temperatures) == 0 {
		return 0, errors.New("no CPU package temperature found in hwmon")
	}
	return maxFloat(temperatures), nil
}

func supportedCPUHwmon(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "coretemp", "k10temp", "zenpower":
		return true
	default:
		return false
	}
}

func isPackageTemperature(label string) bool {
	label = strings.ToLower(strings.TrimSpace(label))
	return strings.HasPrefix(label, "package id") || label == "tctl" || label == "tdie"
}

func readTrimmed(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func maxFloat(values []float64) float64 {
	maximum := values[0]
	for _, value := range values[1:] {
		if value > maximum {
			maximum = value
		}
	}
	return maximum
}

const (
	absoluteMinimumTemperature = -20
	maximumValidTemperature    = 150
)

func validTemperature(temperature, minimum float64) bool {
	return temperature >= minimum && temperature <= maximumValidTemperature && !math.IsNaN(temperature)
}
