package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
)

var sensorReadingPattern = regexp.MustCompile(`(?i)Sensor Reading\s*:\s*([+-]?[0-9]+(?:\.[0-9]+)?)`)

type IPMIClient struct {
	cfg    CommandConfig
	runner CommandRunner
	log    *slog.Logger
	dryRun bool
}

func NewIPMIClient(cfg CommandConfig, runner CommandRunner, logger *slog.Logger, dryRun bool) *IPMIClient {
	return &IPMIClient{cfg: cfg, runner: runner, log: logger, dryRun: dryRun}
}

func (client *IPMIClient) GetFanMode(ctx context.Context) (int, error) {
	output, err := client.raw(ctx, "0x30", "0x45", "0x00")
	if err != nil {
		return 0, err
	}
	return parseHexByte(output)
}

func (client *IPMIClient) SetFanMode(ctx context.Context, mode int) error {
	if client.dryRun {
		client.log.Info("fan mode change skipped", "event", "dry_run", "mode", mode)
		return nil
	}
	_, err := client.raw(ctx, "0x30", "0x45", "0x01", hexByte(mode))
	if err == nil {
		client.log.Info("fan mode written", "event", "ipmi_mode_write", "mode", mode)
	}
	return err
}

func (client *IPMIClient) GetZoneDuty(ctx context.Context, zone int) (int, error) {
	output, err := client.raw(ctx, "0x30", "0x70", "0x66", "0x00", hexByte(zone))
	if err != nil {
		return 0, err
	}
	return parseHexByte(output)
}

func (client *IPMIClient) SetZoneDuty(ctx context.Context, zone, duty int) error {
	if client.dryRun {
		client.log.Info("fan duty change skipped", "event", "dry_run", "zone", zone, "duty", duty)
		return nil
	}
	_, err := client.raw(ctx, "0x30", "0x70", "0x66", "0x01", hexByte(zone), hexByte(duty))
	if err == nil {
		client.log.Info("fan duty written", "event", "ipmi_duty_write", "zone", zone, "duty", duty)
	}
	return err
}

func (client *IPMIClient) ReadTemperature(ctx context.Context, sensor string) (float64, error) {
	return client.readNumericSensor(ctx, sensor)
}

func (client *IPMIClient) ReadFanRPM(ctx context.Context, sensor string) (float64, error) {
	return client.readNumericSensor(ctx, sensor)
}

func (client *IPMIClient) readNumericSensor(ctx context.Context, sensor string) (float64, error) {
	result, err := client.run(ctx, "sensor", "get", sensor)
	if err != nil {
		return 0, err
	}
	if result.ExitCode != 0 {
		return 0, commandExitError(client.cfg.Path, result)
	}
	match := sensorReadingPattern.FindSubmatch(result.Stdout)
	if len(match) != 2 {
		return 0, fmt.Errorf("IPMI sensor %q has no numeric reading", sensor)
	}
	temperature, err := strconv.ParseFloat(string(match[1]), 64)
	if err != nil {
		return 0, fmt.Errorf("error parsing IPMI sensor %q: %w", sensor, err)
	}
	return temperature, nil
}

func (client *IPMIClient) raw(ctx context.Context, args ...string) ([]byte, error) {
	result, err := client.run(ctx, append([]string{"raw"}, args...)...)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, commandExitError(client.cfg.Path, result)
	}
	return result.Stdout, nil
}

func (client *IPMIClient) run(ctx context.Context, args ...string) (CommandResult, error) {
	commandCtx, cancel := context.WithTimeout(ctx, client.cfg.Timeout.Duration)
	defer cancel()
	allArgs := append(append([]string{}, client.cfg.Args...), args...)
	result, err := client.runner.Run(commandCtx, client.cfg.Path, allArgs...)
	if err != nil {
		return result, fmt.Errorf("error running %s: %w", client.cfg.Path, err)
	}
	if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
		return result, fmt.Errorf("%s timed out after %s", client.cfg.Path, client.cfg.Timeout.Duration)
	}
	return result, nil
}

func parseHexByte(output []byte) (int, error) {
	fields := strings.Fields(string(output))
	if len(fields) == 0 {
		return 0, errors.New("IPMI raw response is empty")
	}
	value := strings.TrimPrefix(strings.ToLower(fields[0]), "0x")
	parsed, err := strconv.ParseUint(value, 16, 8)
	if err != nil {
		return 0, fmt.Errorf("invalid IPMI byte %q", fields[0])
	}
	return int(parsed), nil
}

func hexByte(value int) string {
	return fmt.Sprintf("0x%02x", value)
}

func commandExitError(path string, result CommandResult) error {
	message := strings.TrimSpace(string(result.Stderr))
	if message == "" {
		message = strings.TrimSpace(string(result.Stdout))
	}
	if message == "" {
		return fmt.Errorf("%s exited with status %d", path, result.ExitCode)
	}
	return fmt.Errorf("%s exited with status %d: %s", path, result.ExitCode, message)
}
