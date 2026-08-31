package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/kardianos/service"
	"gopkg.in/yaml.v3"
)

const (
	serviceName        = "HarmonyAgent"
	serviceDisplayName = "Harmony Agent"
	serviceDescription = "Publishes Windows telemetry to Harmony over MQTT."

	defaultConfigFile          = "windows.yaml"
	telemetryTopic             = "/iot"
	defaultPollIntervalSeconds = 30
	serviceStopTimeout         = 10 * time.Second

	counterCPU    = `\Processor(_Total)\% Processor Time`
	counterMemory = `\Memory\% Committed Bytes In Use`
	counterDisk   = `\PhysicalDisk(_Total)\% Disk Time`
)

type Config struct {
	Broker              string `yaml:"Broker"`
	Username            string `yaml:"Username"`
	Password            string `yaml:"Password"`
	DeviceID            string `yaml:"DeviceID"`
	PollIntervalSeconds int    `yaml:"PollIntervalSeconds"`
}

type Event struct {
	Name      string `json:"name"`
	Timestamp string `json:"timestamp"`
	Value     any    `json:"value"`
}

type Payload struct {
	DeviceID string  `json:"deviceId"`
	Events   []Event `json:"events"`
}

type program struct {
	configPath string

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func main() {
	serviceAction := flag.String("service", "", "service command: install, uninstall, start, stop, restart")
	configFlag := flag.String("config", "", "path to windows.yaml")
	flag.Parse()

	configPath, err := resolveConfigPath(*configFlag)
	if err != nil {
		slog.Error("failed to resolve config path", "error", err)
		os.Exit(1)
	}

	svcConfig := &service.Config{
		Name:        serviceName,
		DisplayName: serviceDisplayName,
		Description: serviceDescription,
		Arguments:   []string{"-config", configPath},
		Option: service.KeyValue{
			"StartType":        "automatic",
			"DelayedAutoStart": true,
		},
	}

	prg := &program{configPath: configPath}
	svc, err := service.New(prg, svcConfig)
	if err != nil {
		slog.Error("failed to create service", "error", err)
		os.Exit(1)
	}

	if *serviceAction != "" {
		if err := service.Control(svc, *serviceAction); err != nil {
			slog.Error("service command failed", "command", *serviceAction, "error", err)
			os.Exit(1)
		}
		return
	}

	if service.Interactive() {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		if err := run(ctx, configPath); err != nil {
			slog.Error("agent failed", "error", err)
			os.Exit(1)
		}
		return
	}

	if err := svc.Run(); err != nil {
		slog.Error("service failed", "error", err)
		os.Exit(1)
	}
}

func (p *program) Start(service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	p.mu.Lock()
	p.cancel = cancel
	p.done = done
	p.mu.Unlock()

	go func() {
		defer close(done)
		if err := run(ctx, p.configPath); err != nil {
			slog.Error("agent failed", "error", err)
		}
	}()

	return nil
}

func (p *program) Stop(service.Service) error {
	p.mu.Lock()
	cancel := p.cancel
	done := p.done
	p.mu.Unlock()

	if cancel == nil || done == nil {
		return nil
	}

	cancel()
	select {
	case <-done:
	case <-time.After(serviceStopTimeout):
		slog.Warn("timed out waiting for agent to stop")
	}

	return nil
}

func resolveConfigPath(configPath string) (string, error) {
	configPath = strings.TrimSpace(configPath)
	if configPath != "" {
		return filepath.Abs(configPath)
	}

	exePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}

	return filepath.Join(filepath.Dir(exePath), defaultConfigFile), nil
}

func run(ctx context.Context, configPath string) error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("this program only runs on windows: %s", runtime.GOOS)
	}

	config, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	brokerOptions := mqtt.NewClientOptions()
	brokerOptions.AddBroker(normalizeBroker(config.Broker))
	brokerOptions.SetClientID(config.DeviceID)
	brokerOptions.SetAutoReconnect(true)
	brokerOptions.SetCleanSession(false)
	brokerOptions.SetResumeSubs(true)
	brokerOptions.SetUsername(config.Username)
	brokerOptions.SetPassword(config.Password)

	client := mqtt.NewClient(brokerOptions)
	connectToken := client.Connect()
	if !connectToken.WaitTimeout(10 * time.Second) {
		return fmt.Errorf("mqtt connect timed out")
	}
	if err := connectToken.Error(); err != nil {
		return fmt.Errorf("mqtt connect: %w", err)
	}
	defer client.Disconnect(250)

	runPollCycle(ctx, client, config)

	pollTicker := time.NewTicker(time.Duration(config.PollIntervalSeconds) * time.Second)
	defer pollTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-pollTicker.C:
			runPollCycle(ctx, client, config)
		}
	}
}

func loadConfig(configPath string) (Config, error) {
	file, err := os.ReadFile(configPath)
	if err != nil {
		return Config{}, fmt.Errorf("open config file %s: %w", configPath, err)
	}

	var config Config
	if err := yaml.Unmarshal(file, &config); err != nil {
		return Config{}, fmt.Errorf("unmarshal config file %s: %w", configPath, err)
	}

	config.Broker = strings.TrimSpace(config.Broker)
	config.Username = strings.TrimSpace(config.Username)
	config.DeviceID = strings.TrimSpace(config.DeviceID)
	if config.PollIntervalSeconds <= 0 {
		config.PollIntervalSeconds = defaultPollIntervalSeconds
	}

	if config.Broker == "" {
		return Config{}, fmt.Errorf("Broker is required")
	}
	if config.DeviceID == "" {
		return Config{}, fmt.Errorf("DeviceID is required")
	}

	return config, nil
}

func normalizeBroker(broker string) string {
	if strings.HasPrefix(strings.ToLower(broker), "mqtts://") {
		return "ssl://" + broker[len("mqtts://"):]
	}
	if strings.HasPrefix(strings.ToLower(broker), "mqtt://") {
		return "tcp://" + broker[len("mqtt://"):]
	}
	return broker
}

func runPollCycle(ctx context.Context, client mqtt.Client, config Config) {
	cpu, err := getPerformanceCounter(ctx, counterCPU)
	if err != nil {
		slog.Warn("failed to read cpu counter", "error", err)
		return
	}

	memory, err := getPerformanceCounter(ctx, counterMemory)
	if err != nil {
		slog.Warn("failed to read memory counter", "error", err)
		return
	}

	disk, err := getPerformanceCounter(ctx, counterDisk)
	if err != nil {
		slog.Warn("failed to read disk counter", "error", err)
		return
	}

	events := []Event{
		newEvent("connected", 1),
		newEvent("networkStatus", 1),
		newEvent("cpuUtilization", formatFloat(cpu)),
		newEvent("memoryUtilization", formatFloat(memory)),
		newEvent("diskUtilization", formatFloat(disk)),
	}
	publishEvents(client, config, events)
	slog.Info("published telemetry", "cpu", formatFloat(cpu), "memory", formatFloat(memory), "disk", formatFloat(disk))
}

func publishEvents(client mqtt.Client, config Config, events []Event) {
	payload := Payload{
		DeviceID: config.DeviceID,
		Events:   events,
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		slog.Error("failed to marshal payload", "error", err)
		return
	}

	token := client.Publish(telemetryTopic, 0, false, string(raw))
	token.Wait()
	if token.Error() != nil {
		slog.Error("failed to publish mqtt message", "error", token.Error())
	}
}

func newEvent(name string, value any) Event {
	return Event{
		Name:      name,
		Value:     value,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
}

func getPerformanceCounter(ctx context.Context, counter string) (float64, error) {
	cmd := exec.CommandContext(ctx, "typeperf", counter, "-sc", "1")
	output, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("typeperf failed: %w", err)
	}

	lines := strings.Split(string(output), "\n")
	if len(lines) < 3 {
		return 0, fmt.Errorf("unexpected output from typeperf")
	}

	values := strings.Split(lines[2], ",")
	if len(values) < 2 {
		return 0, fmt.Errorf("unexpected output from typeperf")
	}

	valueRaw := strings.Trim(values[1], "\"\r\n ")
	value, err := strconv.ParseFloat(valueRaw, 64)
	if err != nil {
		return 0, fmt.Errorf("parse typeperf value: %w", err)
	}

	return value, nil
}

func formatFloat(value float64) string {
	return fmt.Sprintf("%.2f", value)
}
