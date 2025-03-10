package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/docker/docker/client"
	"github.com/ilyakaznacheev/cleanenv"
	"gopkg.in/yaml.v3"
)

// Settings and state of the application
type Settings struct {
	SettingsFile       SettingsFile
	SettingsWrite      bool
	SettingsExists     bool
	SettingsFilePath   string
	Command            string
	InternalHostDomain string
	Containers
}

type Containers struct {
	Crux           ContainerSettings
	CruxMigrate    ContainerSettings
	CruxUI         ContainerSettings
	Traefik        ContainerSettings
	Kratos         ContainerSettings
	KratosMigrate  ContainerSettings
	CruxPostgres   ContainerSettings
	KratosPostgres ContainerSettings
	MailSlurper    ContainerSettings
}

type ContainerSettings struct {
	Image      string
	Name       string
	Disabled   bool
	CruxAddr   string
	CruxUIPort uint
}

// Settings file will be read/written as this struct
type SettingsFile struct {
	// version as in image tag like "latest" or "stable"
	Version        string `yaml:"version" env-default:"latest"`
	CruxDisabled   bool   `yaml:"crux_disabled" env-default:"false"`
	CruxUIDisabled bool   `yaml:"crux-ui_disabled" env-default:"false"`
	Network        string `yaml:"network-name" env-default:"dyrectorio-stack"`
	Prefix         string `yaml:"prefix" env-default:"dyrectorio-stack"`
	Options
}

type Options struct {
	TimeZone                       string `yaml:"timezone" env-default:"Europe/Budapest"`
	CruxAgentGrpcPort              uint   `yaml:"crux-agentgrpc-port" env-default:"5000"`
	CruxGrpcPort                   uint   `yaml:"crux-grpc-port" env-default:"5001"`
	CruxUIPort                     uint   `yaml:"crux-ui-port" env-default:"3000"`
	CruxSecret                     string `yaml:"crux-secret"`
	CruxPostgresPort               uint   `yaml:"cruxPostgresPort" env-default:"5432"`
	CruxPostgresDB                 string `yaml:"cruxPostgresDB" env-default:"crux"`
	CruxPostgresUser               string `yaml:"cruxPostgresUser" env-default:"crux"`
	CruxPostgresPassword           string `yaml:"cruxPostgresPassword"`
	TraefikWebPort                 uint   `yaml:"traefikWebPort" env-default:"8000"`
	TraefikUIPort                  uint   `yaml:"traefikUIPort" env-default:"8080"`
	TraefikDockerSocket            string `yaml:"traefikDockerSocket" env-default:"/var/run/docker.sock"`
	TraefikIsDockerSocketNamedPipe bool   `yaml:"traefikIsDockerSocketNamedPipe" env-default:"false"`
	KratosAdminPort                uint   `yaml:"kratosAdminPort" env-default:"4434"`
	KratosPublicPort               uint   `yaml:"kratosPublicPort" env-default:"4433"`
	KratosPostgresPort             uint   `yaml:"kratosPostgresPort" env-default:"5433"`
	KratosPostgresDB               string `yaml:"kratosPostgresDB" env-default:"kratos"`
	KratosPostgresUser             string `yaml:"kratosPostgresUser" env-default:"kratos"`
	KratosPostgresPassword         string `yaml:"kratosPostgresPassword"`
	KratosSecret                   string `yaml:"kratosSecret"`
	MailSlurperPort                uint   `yaml:"mailSlurperPort" env-default:"4436"`
	MailSlurperPort2               uint   `yaml:"mailSlurperPort2" env-default:"4437"`
}

const DefaultPostgresPort = 5432

const SecretLength = 32

const BufferMultiplier = 2

const FilePerms = 0600
const DirPerms = 0750

const SettingsFileName = "settings.yaml"
const SettingsFileDir = "dyo-cli"

const (
	CruxAgentGrpcPort  = "CruxAgentGrpcPort"
	CruxGrpcPort       = "CruxGrpcPort"
	CruxUIPort         = "CruxUIPort"
	KratosAdminPort    = "KratosAdminPort"
	KratosPublicPort   = "KratosPublicPort"
	KratosPostgresPort = "KratosPostgresPort"
	MailSlurperPort    = "MailSlurperPort"
	MailSlurperPort2   = "MailSlurperPort2"
	CruxPostgresPort   = "CruxPostgresPort"
	TraefikWebPort     = "TraeficWebPort"
	TraefikUIPort      = "TraefikUIPort"
)

const (
	ParseBase    = 10
	ParseBitSize = 32
)

// Check if the settings file is exists
func SettingsExists(settingspath string) bool {
	settingsfilepath := SettingsFileLocation(settingspath)

	if _, err := os.Stat(settingsfilepath); err == nil {
		return true
	} else if errors.Is(err, os.ErrNotExist) {
		return false
	} else {
		log.Fatalf("%s", err)
		return false
	}
}

// Assemble the location of the settings file
func SettingsFileLocation(settingspath string) string {
	if settingspath == "" {
		userconfdir, err := os.UserConfigDir()
		if err != nil {
			log.Fatalf("Couldn't determine the user's configuration dir: %s", err)
		}
		settingspath = fmt.Sprintf("%s/%s/%s", userconfdir, SettingsFileDir, SettingsFileName)
	}

	return settingspath
}

// Reading and parsing the settings.yaml
func SettingsFileReadWrite(state *Settings) *Settings {
	if state.SettingsExists {
		err := cleanenv.ReadConfig(state.SettingsFilePath, &state.SettingsFile)
		if err != nil {
			log.Fatalf("error: failed to load configuration: %v", err)
		}
	} else {
		state.SettingsWrite = true
		err := cleanenv.ReadEnv(&state.SettingsFile)
		if err != nil {
			log.Fatalf("error: failed to load configuration: %v", err)
		}
	}

	internalHostDomain := CheckRequirements()

	// Fill out data if empty
	settings := LoadDefaultsOnEmpty(state)
	settings.InternalHostDomain = internalHostDomain

	EnsureNetworkExists(settings)

	// Move other values
	settings.Containers.CruxUI.CruxUIPort = settings.SettingsFile.CruxUIPort

	// Set disabled stuff
	settings = DisabledServiceSettings(settings)

	// Settings Validation steps

	SaveSettings(settings)

	return settings
}

// Check prerequisites
func CheckRequirements() string {
	// getenv
	envVarValue := os.Getenv("DOCKER_HOST")

	if envVarValue != "" {
		socketurl, err := url.ParseRequestURI(envVarValue)
		if err != nil {
			log.Fatalf("error: %v", err)
		}

		if socketurl.Host != "" {
			log.Fatalf("error: DOCKER_HOST variable shouldn't have host")
		}

		if socketurl.Scheme != "unix" {
			log.Fatalf("error: DOCKER_HOST variable should contain a valid unix socket")
		}
	} else {
		// We cannot assume unix:///var/run/docker.sock on Mac/Win platforms, we let Docker SDK does its magic :)
		log.Println("\033[33mwarning: DOCKER_HOST environmental variable is empty or not set.\033[0m")
		log.Println("\033[33mUsing default socket determined by Docker SDK\033[0m")
	}

	// Check socket
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("docker socket connection unsuccessful: %v", err)
	}

	info, err := cli.Info(context.Background())
	if err != nil {
		log.Fatalf("cannot get info via docker socket: %v", err)
	}

	switch info.InitBinary {
	case "":
		log.Printf("podman version: %s", info.ServerVersion)
		PodmanInfo()
		return PodmanHost
	case "docker-init":
		log.Printf("docker version: %s", info.ServerVersion)
		return DockerHost
	default:
		log.Fatalf("unknown init binary")
		return ""
	}
}

func PodmanInfo() {
	cmd := exec.Command("podman", "info", "--format", "{{.Host.NetworkBackend}}")

	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Fatalf("podman check stderr pipe error: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatalf("podman check stdout pipe error: %v", err)
	}

	err = cmd.Start()
	if err != nil {
		log.Fatalf("podman command execution error: %v", err)
	}

	readstderr, err := io.ReadAll(stderr)
	if err != nil {
		log.Fatalf("podman command stderr reading error: %v", err)
	}

	readstdout, err := io.ReadAll(stdout)
	if err != nil {
		log.Fatalf("podman command stderr reading error: %v", err)
	}

	err = cmd.Wait()
	if err != nil {
		log.Fatalf("podman command execution error: %v", err)
	}

	if len(readstderr) != 0 {
		log.Fatalf("podman command has errors: %s", string(readstderr))
	}

	if string(readstdout) != "netavark\n" {
		log.Fatalf("podman network backend error: it should have the netavark network backend")
	}
}

func DisabledServiceSettings(settings *Settings) *Settings {
	if settings.Containers.Crux.Disabled {
		fmt.Printf("Do not forget to add your DATABASE_URL to your crux environment.\n\n")
		fmt.Printf("DATABASE_URL=postgresql://%s:%s@%s_crux-postgres:%d/%s?schema=public\n\n",
			settings.SettingsFile.CruxPostgresUser,
			settings.SettingsFile.CruxPostgresPassword,
			settings.SettingsFile.Prefix,
			settings.SettingsFile.CruxPostgresPort,
			settings.SettingsFile.CruxPostgresDB)
	}

	if settings.Containers.CruxUI.Disabled {
		settings.CruxUI.CruxAddr = "localhost"
	} else {
		settings.CruxUI.CruxAddr = fmt.Sprintf("%s_crux", settings.SettingsFile.Prefix)
	}

	return settings
}

// Save the settings
func SaveSettings(settings *Settings) {
	if settings.SettingsWrite {
		userconfdir, _ := os.UserConfigDir()
		settingspath := fmt.Sprintf("%s/%s/%s", userconfdir, SettingsFileDir, SettingsFileName)

		// If settingspath is default, we create the directory for it
		if settings.SettingsFilePath == settingspath {
			if _, err := os.Stat(filepath.Dir(settingspath)); errors.Is(err, os.ErrNotExist) {
				err = os.Mkdir(filepath.Dir(settingspath), DirPerms)
				if err != nil {
					log.Fatalf("%v", err)
				}
			} else if err != nil {
				log.Fatalf("%v", err)
			}
		}

		filedata, err := yaml.Marshal(&settings.SettingsFile)
		if err != nil {
			log.Fatalf("%v", err)
		}

		err = os.WriteFile(settings.SettingsFilePath, filedata, FilePerms)
		if err != nil {
			log.Fatalf("%v", err)
		}

		settings.SettingsWrite = false
	}
}

// There are options which are not filled out by default, we need to initialize values
func LoadDefaultsOnEmpty(settings *Settings) *Settings {
	// Set Docker Image location
	settings.Crux.Image = "ghcr.io/dyrector-io/dyrectorio/web/crux"
	settings.CruxUI.Image = "ghcr.io/dyrector-io/dyrectorio/web/crux-ui"
	settings.Kratos.Image = "ghcr.io/dyrector-io/dyrectorio/web/kratos"

	// Store state to settings
	if settings.Containers.Crux.Disabled != settings.SettingsFile.CruxDisabled {
		settings.SettingsFile.CruxDisabled = settings.Containers.Crux.Disabled
	}
	if settings.Containers.CruxUI.Disabled != settings.SettingsFile.CruxUIDisabled {
		settings.SettingsFile.CruxUIDisabled = settings.Containers.CruxUI.Disabled
	}

	// Load defaults
	settings.SettingsFile.CruxSecret = LoadStringVal(settings.SettingsFile.CruxSecret, RandomChars(SecretLength))
	settings.SettingsFile.CruxPostgresPassword = LoadStringVal(settings.SettingsFile.CruxPostgresPassword, RandomChars(SecretLength))
	settings.SettingsFile.KratosPostgresPassword = LoadStringVal(settings.SettingsFile.KratosPostgresPassword, RandomChars(SecretLength))
	settings.SettingsFile.KratosSecret = LoadStringVal(settings.SettingsFile.KratosSecret, RandomChars(SecretLength))

	// Generate names
	settings.Containers.Traefik.Name = fmt.Sprintf("%s_traefik", settings.SettingsFile.Prefix)
	settings.Containers.Crux.Name = fmt.Sprintf("%s_crux", settings.SettingsFile.Prefix)
	settings.Containers.CruxMigrate.Name = fmt.Sprintf("%s_crux-migrate", settings.SettingsFile.Prefix)
	settings.Containers.CruxUI.Name = fmt.Sprintf("%s_crux-ui", settings.SettingsFile.Prefix)
	settings.Containers.Kratos.Name = fmt.Sprintf("%s_kratos", settings.SettingsFile.Prefix)
	settings.Containers.KratosMigrate.Name = fmt.Sprintf("%s_kratos-migrate", settings.SettingsFile.Prefix)
	settings.Containers.CruxPostgres.Name = fmt.Sprintf("%s_crux-postgres", settings.SettingsFile.Prefix)
	settings.Containers.KratosPostgres.Name = fmt.Sprintf("%s_kratos-postgres", settings.SettingsFile.Prefix)
	settings.Containers.MailSlurper.Name = fmt.Sprintf("%s_mailslurper", settings.SettingsFile.Prefix)

	return settings
}

func LoadStringVal(value, def string) string {
	if value == "" {
		return def
	}
	return value
}

func RandomChars(bufflength uint) string {
	buffer := make([]byte, bufflength*BufferMultiplier)
	_, err := rand.Read(buffer)
	if err != nil {
		log.Fatalf("error: %v", err)
	}
	secureString := make([]byte, base64.StdEncoding.EncodedLen(len(buffer)))
	base64.StdEncoding.Encode(secureString, buffer)

	result := strings.ReplaceAll(
		strings.ReplaceAll(
			strings.ReplaceAll(
				string(secureString),
				"+", ""),
			"/", ""),
		"=", "")

	return result[0:bufflength]
}

func CheckAndUpdatePorts(settings *Settings) *Settings {
	portMap := map[string]uint{}
	if !settings.Containers.Crux.Disabled {
		portMap[CruxAgentGrpcPort] = getAvailablePort(portMap, settings.SettingsFile.Options.CruxAgentGrpcPort,
			CruxAgentGrpcPort, &settings.SettingsWrite)
		settings.SettingsFile.Options.CruxAgentGrpcPort = portMap[CruxAgentGrpcPort]
		portMap[CruxGrpcPort] = getAvailablePort(portMap, settings.SettingsFile.Options.CruxGrpcPort,
			CruxGrpcPort, &settings.SettingsWrite)
		settings.SettingsFile.Options.CruxGrpcPort = portMap[CruxGrpcPort]
	}
	if !settings.Containers.CruxUI.Disabled {
		portMap[CruxUIPort] = getAvailablePort(portMap, settings.SettingsFile.Options.CruxUIPort,
			CruxUIPort, &settings.SettingsWrite)
		settings.SettingsFile.Options.CruxUIPort = portMap[CruxUIPort]
	}

	portMap[CruxPostgresPort] = getAvailablePort(portMap, settings.SettingsFile.Options.CruxPostgresPort,
		CruxPostgresPort, &settings.SettingsWrite)
	settings.SettingsFile.Options.CruxPostgresPort = portMap[CruxPostgresPort]
	portMap[KratosAdminPort] = getAvailablePort(portMap, settings.SettingsFile.Options.KratosAdminPort,
		KratosAdminPort, &settings.SettingsWrite)
	settings.SettingsFile.Options.KratosAdminPort = portMap[KratosAdminPort]
	portMap[KratosPublicPort] = getAvailablePort(portMap, settings.SettingsFile.Options.KratosPublicPort,
		KratosPublicPort, &settings.SettingsWrite)
	settings.SettingsFile.Options.KratosPublicPort = portMap[KratosPublicPort]
	portMap[KratosPostgresPort] = getAvailablePort(portMap, settings.SettingsFile.Options.KratosPostgresPort,
		KratosPostgresPort, &settings.SettingsWrite)
	settings.SettingsFile.Options.KratosPostgresPort = portMap[KratosPostgresPort]
	portMap[MailSlurperPort] = getAvailablePort(portMap, settings.SettingsFile.Options.MailSlurperPort,
		MailSlurperPort, &settings.SettingsWrite)
	settings.SettingsFile.Options.MailSlurperPort = portMap[MailSlurperPort]
	portMap[MailSlurperPort2] = getAvailablePort(portMap, settings.SettingsFile.Options.MailSlurperPort2,
		MailSlurperPort2, &settings.SettingsWrite)
	settings.SettingsFile.Options.MailSlurperPort2 = portMap[MailSlurperPort2]
	portMap[TraefikWebPort] = getAvailablePort(portMap, settings.SettingsFile.Options.TraefikWebPort,
		TraefikWebPort, &settings.SettingsWrite)
	settings.SettingsFile.Options.TraefikWebPort = portMap[TraefikWebPort]
	portMap[TraefikUIPort] = getAvailablePort(portMap, settings.SettingsFile.Options.TraefikUIPort,
		TraefikUIPort, &settings.SettingsWrite)
	settings.SettingsFile.Options.TraefikUIPort = portMap[TraefikUIPort]

	return settings
}

func getAvailablePort(portMap map[string]uint, portNum uint, portDesc string, changed *bool) uint {
	for {
		if err := portIsAvailable(portMap, portNum); err != nil {
			fmt.Fprintf(os.Stderr, "error in binding port for %s: %s\n", portDesc, err.Error())
			fmt.Fprintf(os.Stdout, "type another port: ")
			portNum = scanPort(portNum)
			*changed = true
			continue
		}
		break
	}
	return portNum
}

func scanPort(portNum uint) uint {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		newPort, err := strconv.ParseUint(scanner.Text(), ParseBase, ParseBitSize)
		if err != nil || (newPort > 0 && newPort <= 1023) || newPort == 0 {
			fmt.Fprintf(os.Stderr, "you typed invalid port number:\n")
			fmt.Fprintf(os.Stdout, "type another port: ")
			continue
		}
		return uint(newPort)
	}
	return portNum
}

func portIsAvailable(portMap map[string]uint, portNum uint) error {
	err := portIsAvailableOnHost(portNum)
	if err == nil {
		err = externalPortIsDuplicated(portMap, portNum)
	}
	return err
}

func portIsAvailableOnHost(portNum uint) error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", portNum))
	if err != nil {
		return fmt.Errorf("can`t bind, %w", err)
	}

	err = ln.Close()
	if err != nil {
		return fmt.Errorf("can`t close, %w", err)
	}
	return nil
}

func externalPortIsDuplicated(portMap map[string]uint, candidatePort uint) error {
	for desc, port := range portMap {
		if port == candidatePort {
			return fmt.Errorf("port %d is used by %s", port, desc)
		}
	}
	return nil
}
