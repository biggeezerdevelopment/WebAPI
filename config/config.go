package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Header struct {
	Key   string `yaml:"key"`
	Value string `yaml:"value"`
}

type WebAPI struct {
	Enabled     bool              `yaml:"enabled"`
	Name        string            `yaml:"name"`
	Subject     string            `yaml:"subject"`
	Endpoint    string            `yaml:"endpoint"`
	Method      string            `yaml:"method"`
	Headers     []Header          `yaml:"headers"`
	AuthType    string            `yaml:"auth_type"`
	APIKey      string            `yaml:"api_key"`
	APIUsername string            `yaml:"api_username"`
	APIPassword string            `yaml:"api_password"`
	SkipVerify  bool              `yaml:"skip_verify"`
	SleepTime   int               `yaml:"sleeptime"`
	Timeout     int               `yaml:"timeout"`
	Body        string            `yaml:"body"`
	Params      map[string]string `yaml:"params"`
}

type APILogRemote struct {
	Enabled    bool     `yaml:"enabled"`
	APIURL     string   `yaml:"api_url"`
	APIMethod  string   `yaml:"api_method"`
	APIHeaders []Header `yaml:"api_headers"`
	SkipVerify bool     `yaml:"skip_verify"`
}

type JarvisAgent struct {
	Name string `yaml:"name"`
	ID   string `yaml:"id"`
}

type Config struct {
	JarvisAgent  JarvisAgent  `yaml:"jarvisagent"`
	APILogRemote APILogRemote `yaml:"apilogremote"`
	WebAPI       []WebAPI     `yaml:"webapi"`
}

func LoadConfig(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("error reading config file: %v", err)
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("error parsing config file: %v", err)
	}

	return &config, nil
}
