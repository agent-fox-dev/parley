// Package kubernetes holds the manifests that run one hub in a cluster. They
// are checked by decoding them, so no cluster is needed.
package kubernetes

import (
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/agent-fox-dev/parley/internal/config"
)

// meta is the part of every object the tests read.
type meta struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name   string            `yaml:"name"`
		Labels map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
}

type configMap struct {
	Data map[string]string `yaml:"data"`
}

type secret struct {
	StringData map[string]string `yaml:"stringData"`
}

type pvc struct {
	Spec struct {
		AccessModes []string `yaml:"accessModes"`
		Resources   struct {
			Requests map[string]string `yaml:"requests"`
		} `yaml:"resources"`
		StorageClassName *string `yaml:"storageClassName"`
	} `yaml:"spec"`
}

// port is a port number or the name of a container's port.
type port struct {
	Number int
	Name   string
}

func (p *port) UnmarshalYAML(n *yaml.Node) error {
	if err := n.Decode(&p.Number); err == nil {
		return nil
	}
	return n.Decode(&p.Name)
}

type probe struct {
	HTTPGet *struct {
		Path string `yaml:"path"`
		Port port   `yaml:"port"`
	} `yaml:"httpGet"`
}

type mount struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
	SubPath   string `yaml:"subPath"`
}

type containerPort struct {
	Name          string `yaml:"name"`
	ContainerPort int    `yaml:"containerPort"`
}

type envSource struct {
	SecretRef *struct {
		Name string `yaml:"name"`
	} `yaml:"secretRef"`
}

type container struct {
	Name            string          `yaml:"name"`
	Image           string          `yaml:"image"`
	Command         []string        `yaml:"command"`
	Args            []string        `yaml:"args"`
	Ports           []containerPort `yaml:"ports"`
	EnvFrom         []envSource     `yaml:"envFrom"`
	VolumeMounts    []mount         `yaml:"volumeMounts"`
	StartupProbe    probe           `yaml:"startupProbe"`
	LivenessProbe   probe           `yaml:"livenessProbe"`
	ReadinessProbe  probe           `yaml:"readinessProbe"`
	SecurityContext struct {
		RunAsUser    *int  `yaml:"runAsUser"`
		RunAsNonRoot *bool `yaml:"runAsNonRoot"`
	} `yaml:"securityContext"`
}

// resolve returns the number p names among the container's ports, or 0.
func (c container) resolve(p port) int {
	if p.Name == "" {
		return p.Number
	}
	for _, cp := range c.Ports {
		if cp.Name == p.Name {
			return cp.ContainerPort
		}
	}
	return 0
}

type deployment struct {
	Spec struct {
		Replicas *int `yaml:"replicas"`
		Strategy struct {
			Type string `yaml:"type"`
		} `yaml:"strategy"`
		Selector struct {
			MatchLabels map[string]string `yaml:"matchLabels"`
		} `yaml:"selector"`
		Template struct {
			Metadata struct {
				Labels map[string]string `yaml:"labels"`
			} `yaml:"metadata"`
			Spec struct {
				TerminationGracePeriodSeconds *int `yaml:"terminationGracePeriodSeconds"`
				SecurityContext               struct {
					RunAsUser    *int  `yaml:"runAsUser"`
					RunAsNonRoot *bool `yaml:"runAsNonRoot"`
					FSGroup      *int  `yaml:"fsGroup"`
				} `yaml:"securityContext"`
				InitContainers []container `yaml:"initContainers"`
				Containers     []container `yaml:"containers"`
				Volumes        []struct {
					Name                  string `yaml:"name"`
					PersistentVolumeClaim *struct {
						ClaimName string `yaml:"claimName"`
					} `yaml:"persistentVolumeClaim"`
					ConfigMap *struct {
						Name string `yaml:"name"`
					} `yaml:"configMap"`
				} `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

type service struct {
	Spec struct {
		Type     string            `yaml:"type"`
		Selector map[string]string `yaml:"selector"`
		Ports    []struct {
			Port       int  `yaml:"port"`
			TargetPort port `yaml:"targetPort"`
		} `yaml:"ports"`
	} `yaml:"spec"`
}

// decodeAll returns every document of the file, one per object.
func decodeAll(t *testing.T, file string) []yaml.Node {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var docs []yaml.Node
	dec := yaml.NewDecoder(f)
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			return docs
		}
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		docs = append(docs, n)
	}
}

// decode decodes the file's one object into out.
func decode(t *testing.T, file string, out any) {
	t.Helper()
	docs := decodeAll(t, file)
	if len(docs) != 1 {
		t.Fatalf("%s holds %d objects, want 1", file, len(docs))
	}
	if err := docs[0].Decode(out); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
}

// hubContainer returns the Deployment and its one container.
func hubContainer(t *testing.T) (deployment, container) {
	t.Helper()
	var d deployment
	decode(t, "deployment.yaml", &d)
	cs := d.Spec.Template.Spec.Containers
	if len(cs) != 1 {
		t.Fatalf("the Deployment has %d containers, want 1", len(cs))
	}
	return d, cs[0]
}

func TestManifests_DefineEachObjectOnceLabelledParley(t *testing.T) {
	t.Parallel()
	tests := []struct{ file, kind, name string }{
		{"configmap.yaml", "ConfigMap", "parley"},
		{"secret.example.yaml", "Secret", "parley-secrets"},
		{"pvc.yaml", "PersistentVolumeClaim", "parley-data"},
		{"deployment.yaml", "Deployment", "parley"},
		{"service.yaml", "Service", "parley"},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			t.Parallel()
			var m meta
			decode(t, tt.file, &m)
			if m.Kind != tt.kind || m.Metadata.Name != tt.name {
				t.Errorf("%s defines %s %q, want %s %q", tt.file, m.Kind, m.Metadata.Name, tt.kind, tt.name)
			}
			if got := m.Metadata.Labels["app.kubernetes.io/name"]; got != "parley" {
				t.Errorf("%s: label app.kubernetes.io/name = %q, want parley", tt.file, got)
			}
		})
	}
}

func TestDeployment_RunsOneReplicaOfTheHubImageWithNoFlags(t *testing.T) {
	t.Parallel()
	d, c := hubContainer(t)
	if r := d.Spec.Replicas; r == nil || *r != 1 {
		t.Errorf("spec.replicas = %v, want 1", r)
	}
	if got := d.Spec.Strategy.Type; got != "Recreate" {
		t.Errorf("spec.strategy.type = %q, want Recreate", got)
	}
	if !strings.Contains(c.Image, "parley-hub") {
		t.Errorf("image = %q, want a parley-hub reference", c.Image)
	}
	if c.Command != nil || c.Args != nil {
		t.Errorf("command = %q, args = %q, want neither, so that the image's entry point runs with no flags", c.Command, c.Args)
	}
}

func TestDeployment_MountsTheClaimAndTheConfigFileAndRunsAsUser10001(t *testing.T) {
	t.Parallel()
	var claim pvc
	decode(t, "pvc.yaml", &claim)
	if got := claim.Spec.AccessModes; !slices.Equal(got, []string{"ReadWriteOnce"}) {
		t.Errorf("claim accessModes = %q, want [ReadWriteOnce]", got)
	}
	if got := claim.Spec.Resources.Requests["storage"]; got != "20Gi" {
		t.Errorf("claim requests storage %q, want 20Gi", got)
	}
	if claim.Spec.StorageClassName != nil {
		t.Errorf("claim names storage class %q, want none", *claim.Spec.StorageClassName)
	}

	d, c := hubContainer(t)
	pod := d.Spec.Template.Spec
	var data, cfg string
	for _, v := range pod.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "parley-data" {
			data = v.Name
		}
		if v.ConfigMap != nil && v.ConfigMap.Name == "parley" {
			cfg = v.Name
		}
	}
	if data == "" || cfg == "" {
		t.Fatalf("pod volumes: claim parley-data in %q, ConfigMap parley in %q; want both", data, cfg)
	}
	for _, want := range []mount{
		{Name: data, MountPath: "/var/lib/parley"},
		{Name: cfg, MountPath: "/var/lib/parley/config/config.toml", SubPath: "config.toml"},
	} {
		if !slices.Contains(c.VolumeMounts, want) {
			t.Errorf("volumeMounts %+v lack %+v", c.VolumeMounts, want)
		}
	}
	sc := pod.SecurityContext
	if sc.RunAsUser == nil || *sc.RunAsUser != 10001 {
		t.Errorf("runAsUser = %v, want 10001", sc.RunAsUser)
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Errorf("runAsNonRoot = %v, want true", sc.RunAsNonRoot)
	}
	if sc.FSGroup == nil || *sc.FSGroup != 10001 {
		t.Errorf("fsGroup = %v, want 10001", sc.FSGroup)
	}

	// The config file's mount would otherwise make the runtime create the
	// config dir, as root, on an empty volume, where the hub could not
	// write its admin token file.
	if len(pod.InitContainers) != 1 {
		t.Fatalf("the Deployment has %d init containers, want 1 that creates the config dir", len(pod.InitContainers))
	}
	ic := pod.InitContainers[0]
	if ic.Image != c.Image {
		t.Errorf("the init container's image = %q, want the hub's %q", ic.Image, c.Image)
	}
	if u := ic.SecurityContext.RunAsUser; u != nil && *u != *sc.RunAsUser {
		t.Errorf("the init container runs as user %d, want the pod's user %d", *u, *sc.RunAsUser)
	}
	if nr := ic.SecurityContext.RunAsNonRoot; nr != nil && !*nr {
		t.Errorf("the init container sets runAsNonRoot false, want it to run as the pod's non-root user")
	}
	if !slices.Contains(ic.VolumeMounts, mount{Name: data, MountPath: "/var/lib/parley"}) {
		t.Errorf("the init container's volumeMounts %+v do not mount the claim at /var/lib/parley", ic.VolumeMounts)
	}
	if ic.Args != nil || len(ic.Command) == 0 || ic.Command[0] != "mkdir" || !slices.Contains(ic.Command, "-p") ||
		!slices.Contains(ic.Command, "/var/lib/parley/config") {
		t.Errorf("the init container's command = %q, args = %q; want mkdir -p of /var/lib/parley/config and no args", ic.Command, ic.Args)
	}
}

func TestConfigMap_LoadsWithTheExampleSecretAsTheEnvironment(t *testing.T) {
	t.Parallel()
	var sec secret
	decode(t, "secret.example.yaml", &sec)
	if len(sec.StringData) == 0 {
		t.Fatal("the example Secret has no stringData")
	}
	for k, v := range sec.StringData {
		if v != "replace-me" {
			t.Errorf("example Secret: %s = %q, want replace-me", k, v)
		}
	}
	_, c := hubContainer(t)
	if !slices.ContainsFunc(c.EnvFrom, func(e envSource) bool {
		return e.SecretRef != nil && e.SecretRef.Name == "parley-secrets"
	}) {
		t.Errorf("envFrom does not name the secret parley-secrets")
	}

	var cm configMap
	decode(t, "configmap.yaml", &cm)
	file, ok := cm.Data["config.toml"]
	if !ok {
		t.Fatal("the ConfigMap has no key config.toml")
	}
	for _, ref := range regexp.MustCompile(`\$\{([^}]*)\}`).FindAllStringSubmatch(file, -1) {
		if _, ok := sec.StringData[ref[1]]; !ok {
			t.Errorf("config.toml refers to ${%s}, which is not a key of the example Secret", ref[1])
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(config.MapEnv(sec.StringData), dir)
	if err != nil {
		t.Fatalf("Load with the example Secret's keys: %v", err)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("port = %d, want 8080", cfg.Server.Port)
	}
	if ps := cfg.OAuth.Providers; len(ps) != 1 || ps[0].Name != "github" || ps[0].ClientSecret != sec.StringData["GITHUB_CLIENT_SECRET"] {
		t.Errorf("providers = %+v, want one github provider whose client secret is GITHUB_CLIENT_SECRET's value", ps)
	}

	if _, err := config.Load(config.MapEnv(nil), dir); err == nil || !strings.Contains(err.Error(), "GITHUB_CLIENT_SECRET") {
		t.Errorf("Load with an empty environment: err = %v, want one naming GITHUB_CLIENT_SECRET", err)
	}
}

func TestDeploymentAndService_ServeAndProbePort8080(t *testing.T) {
	t.Parallel()
	d, c := hubContainer(t)
	if !slices.ContainsFunc(c.Ports, func(p containerPort) bool {
		return p.ContainerPort == 8080
	}) {
		t.Errorf("container ports %+v lack containerPort 8080", c.Ports)
	}
	for _, p := range []struct {
		name, path string
		probe      probe
	}{
		{"startupProbe", "/healthz", c.StartupProbe},
		{"livenessProbe", "/healthz", c.LivenessProbe},
		{"readinessProbe", "/readyz", c.ReadinessProbe},
	} {
		if p.probe.HTTPGet == nil {
			t.Errorf("%s has no httpGet", p.name)
			continue
		}
		if got := p.probe.HTTPGet.Path; got != p.path {
			t.Errorf("%s path = %q, want %s", p.name, got, p.path)
		}
		if got := c.resolve(p.probe.HTTPGet.Port); got != 8080 {
			t.Errorf("%s port %+v resolves to %d, want 8080", p.name, p.probe.HTTPGet.Port, got)
		}
	}
	if g := d.Spec.Template.Spec.TerminationGracePeriodSeconds; g == nil || *g != 40 {
		t.Errorf("terminationGracePeriodSeconds = %v, want 40", g)
	}

	var svc service
	decode(t, "service.yaml", &svc)
	if svc.Spec.Type != "ClusterIP" {
		t.Errorf("Service type = %q, want ClusterIP", svc.Spec.Type)
	}
	if ps := svc.Spec.Ports; len(ps) != 1 || ps[0].Port != 8080 || c.resolve(ps[0].TargetPort) != 8080 {
		t.Errorf("Service ports = %+v, want one port 8080 whose targetPort is the container's port 8080", ps)
	}
	sel := d.Spec.Selector.MatchLabels
	if len(sel) == 0 || !maps.Equal(svc.Spec.Selector, sel) {
		t.Errorf("Service selector = %v, want the Deployment's selector %v", svc.Spec.Selector, sel)
	}
	for k, v := range sel {
		if d.Spec.Template.Metadata.Labels[k] != v {
			t.Errorf("selector label %s=%s is not among the pod template's labels %v", k, v, d.Spec.Template.Metadata.Labels)
		}
	}
}
