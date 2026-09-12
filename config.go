package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

// Subset of the process-compose schema that maps onto tmux windows.
type Proc struct {
	Command      string   `yaml:"command"`
	WorkingDir   string   `yaml:"working_dir"`
	Description  string   `yaml:"description"`
	Namespace    string   `yaml:"namespace"`
	Disabled     bool     `yaml:"disabled"`
	Environment  []string `yaml:"environment"`
	Availability struct {
		Restart string `yaml:"restart"` // always | on_failure | no (default)
	} `yaml:"availability"`

	Name string `yaml:"-"`
}

type Config struct {
	Name      string            `yaml:"name"`
	Vars      map[string]string `yaml:"vars"`
	Processes map[string]*Proc  `yaml:"processes"`

	Order []string `yaml:"-"` // process names in file order
	Dir   string   `yaml:"-"` // directory of the config file
}

var configNames = []string{
	"process-compose-x.yaml", "process-compose-x.yml",
	"process-compose.yaml", "process-compose.yml",
}

// FindConfig walks up from the working directory looking for a config file.
func FindConfig(path string) (string, error) {
	if path != "" {
		return filepath.Abs(path)
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		for _, n := range configNames {
			p := filepath.Join(dir, n)
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no %s found (searched up from the working directory)", configNames[0])
		}
		dir = parent
	}
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	if len(cfg.Processes) == 0 {
		return nil, fmt.Errorf("%s: no processes", path)
	}

	// Second pass purely to keep the file's process order in the TUI.
	var doc struct {
		Processes yaml.Node `yaml:"processes"`
	}
	if err := yaml.Unmarshal(raw, &doc); err == nil {
		for i := 0; i+1 < len(doc.Processes.Content); i += 2 {
			cfg.Order = append(cfg.Order, doc.Processes.Content[i].Value)
		}
	}

	cfg.Dir = filepath.Dir(path)
	if cfg.Name == "" {
		cfg.Name = filepath.Base(cfg.Dir)
	}
	cfg.Name = sanitize(cfg.Name)

	for name, p := range cfg.Processes {
		if strings.ContainsAny(name, ":.") {
			return nil, fmt.Errorf("process %q: name cannot contain ':' or '.' (tmux window name)", name)
		}
		if p.Command == "" {
			return nil, fmt.Errorf("process %q: no command", name)
		}
		p.Name = name
		p.Command = expand(p.Command, cfg.Vars)
		p.WorkingDir = expand(p.WorkingDir, cfg.Vars)
		if p.WorkingDir == "" {
			p.WorkingDir = "."
		}
		if !filepath.IsAbs(p.WorkingDir) {
			p.WorkingDir = filepath.Join(cfg.Dir, p.WorkingDir)
		}
		for i, e := range p.Environment {
			p.Environment[i] = expand(e, cfg.Vars)
		}
		if p.Namespace == "" {
			p.Namespace = "default"
		}
	}
	return &cfg, nil
}

// expand resolves ${ENV} first, then the Go template against vars — the same
// two-step process-compose uses for '{{or "${WORKING_DIR}" .WORKING_DIR}}'.
func expand(s string, vars map[string]string) string {
	if s == "" {
		return s
	}
	s = os.ExpandEnv(s)
	t, err := template.New("v").Parse(s)
	if err != nil {
		return s
	}
	var b bytes.Buffer
	if t.Execute(&b, vars) != nil {
		return s
	}
	return b.String()
}

func sanitize(s string) string {
	return strings.NewReplacer(":", "-", ".", "-", " ", "-").Replace(s)
}

// Namespaces returns namespaces and their processes, in file order.
func (c *Config) Namespaces() ([]string, map[string][]*Proc) {
	var order []string
	byNS := map[string][]*Proc{}
	for _, name := range c.names() {
		p := c.Processes[name]
		if _, ok := byNS[p.Namespace]; !ok {
			order = append(order, p.Namespace)
		}
		byNS[p.Namespace] = append(byNS[p.Namespace], p)
	}
	return order, byNS
}

func (c *Config) names() []string {
	if len(c.Order) == len(c.Processes) {
		return c.Order
	}
	out := make([]string, 0, len(c.Processes))
	for n := range c.Processes {
		out = append(out, n)
	}
	return out
}
