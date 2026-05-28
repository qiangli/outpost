package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
)

// clusterInitCmd scaffolds a Deployment + Service YAML pair with the
// outpost-recommended defaults baked in. Output goes to stdout (or
// --output PATH) ready to pipe into `kubectl apply -f -`.
//
// Why a scaffolder and not a Helm chart: lower barrier (no helm
// install, no chart repo, no values.yaml) and the template is small
// enough to inline. Operators who want richer parameterization
// later can copy the output into their own chart.
//
// What the template bakes in by default:
//
//  - podAntiAffinity (preferred, by hostname) — replicas spread
//    across outposts; single-node clusters still schedule but
//    multi-node clusters get HA for free.
//  - Tolerations for the virtual-kubelet provider taint — without
//    these, k8s refuses to schedule onto vkpodman nodes.
//  - containerPort declared but hostPort left to vkpodman auto-
//    allocate (a7fa651).
//  - readinessProbe (HTTPGet on /, configurable via --probe-path)
//    so cluster-svc only routes to actually-serving pods (56b117c).
//  - Service with outpost.dhnt.io/affinity: user annotation
//    (3532c82) — opt OUT by passing --no-sticky if your app is
//    fully stateless.
//  - Resource requests + limits at sane small defaults; operators
//    can `kubectl edit` higher when they need it.
//  - imagePullPolicy: IfNotPresent — saves bandwidth across pod
//    restarts when the image is unchanged.
func clusterInitCmd() *cobra.Command {
	var (
		name      string
		image     string
		port      int
		replicas  int
		probePath string
		probePort int
		noSticky  bool
		outPath   string
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Scaffold a Deployment + Service YAML with outpost-recommended defaults",
		Long: `Emit a kubectl-ready manifest with anti-affinity, readinessProbe,
tolerations, the cluster-svc affinity annotation, and resource
limits already wired. Pipe to kubectl apply or --output to a file.

Example:

  outpost cluster init --name nginx --image docker.io/library/nginx:alpine \
    --port 80 --replicas 2 | kubectl apply -f -

  Then browse https://ai.dhnt.io/api/cluster/svc/nginx/`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(name) == "" {
				return errors.New("--name required")
			}
			if strings.TrimSpace(image) == "" {
				return errors.New("--image required")
			}
			if port <= 0 || port > 65535 {
				return errors.New("--port must be 1..65535")
			}
			if replicas < 1 {
				replicas = 2
			}
			if probePath == "" {
				probePath = "/"
			}
			if probePort == 0 {
				probePort = port
			}
			data := manifestVars{
				Name:      name,
				Image:     image,
				Port:      port,
				Replicas:  replicas,
				ProbePath: probePath,
				ProbePort: probePort,
				Sticky:    !noSticky,
			}
			var out io.Writer = os.Stdout
			if outPath != "" {
				f, err := os.Create(outPath)
				if err != nil {
					return err
				}
				defer f.Close()
				out = f
			}
			tmpl := template.Must(template.New("manifest").Parse(manifestTemplate))
			if err := tmpl.Execute(out, data); err != nil {
				return err
			}
			if outPath != "" {
				fmt.Fprintf(os.Stderr, "wrote %s — apply with: kubectl apply -f %s\n", outPath, outPath)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "App name (becomes Deployment + Service name + selector label) [required]")
	cmd.Flags().StringVar(&image, "image", "", "Container image including registry + tag [required]")
	cmd.Flags().IntVar(&port, "port", 0, "Container port the app listens on [required]")
	cmd.Flags().IntVar(&replicas, "replicas", 2, "Number of replicas (anti-affinity will spread them across outposts)")
	cmd.Flags().StringVar(&probePath, "probe-path", "/", "HTTP path for the readinessProbe")
	cmd.Flags().IntVar(&probePort, "probe-port", 0, "Port for the readinessProbe (default: --port)")
	cmd.Flags().BoolVar(&noSticky, "no-sticky", false, "Drop the outpost.dhnt.io/affinity: user annotation (use plain RR)")
	cmd.Flags().StringVar(&outPath, "output", "", "Write to PATH instead of stdout")
	return cmd
}

type manifestVars struct {
	Name      string
	Image     string
	Port      int
	Replicas  int
	ProbePath string
	ProbePort int
	Sticky    bool
}

// manifestTemplate is the canonical "good defaults" pair. Indentation
// matters — YAML is whitespace-sensitive — so changes need to be made
// against `kubectl apply --dry-run=client -f -` to confirm validity.
const manifestTemplate = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{.Name}}
  labels:
    app: {{.Name}}
spec:
  replicas: {{.Replicas}}
  selector:
    matchLabels:
      app: {{.Name}}
  template:
    metadata:
      labels:
        app: {{.Name}}
    spec:
      # Toleration for the virtual-kubelet provider taint — required
      # so the scheduler is willing to land pods on vkpodman nodes.
      tolerations:
      - key: virtual-kubelet.io/provider
        operator: Exists
      # Spread replicas across outposts. Preferred (not required) so
      # single-outpost clusters still schedule; once you add more
      # outposts the scheduler naturally rebalances on the next roll.
      affinity:
        podAntiAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
          - weight: 100
            podAffinityTerm:
              labelSelector:
                matchLabels:
                  app: {{.Name}}
              topologyKey: kubernetes.io/hostname
      containers:
      - name: {{.Name}}
        image: {{.Image}}
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: {{.Port}}
          # hostPort left unset — vkpodman auto-allocates a free
          # ephemeral port and labels the container with the
          # mapping so it survives daemon restart.
        readinessProbe:
          httpGet:
            path: {{.ProbePath}}
            port: {{.ProbePort}}
          initialDelaySeconds: 2
          periodSeconds: 5
          timeoutSeconds: 1
          failureThreshold: 3
        resources:
          requests:
            cpu: 50m
            memory: 64Mi
          limits:
            cpu: 500m
            memory: 256Mi
---
apiVersion: v1
kind: Service
metadata:
  name: {{.Name}}
{{- if .Sticky }}
  annotations:
    # Hash the calling user's email onto the live endpoint set —
    # same operator always lands on the same pod while the set is
    # stable. Drop this for fully stateless apps where pure RR is
    # preferable (--no-sticky on outpost cluster init).
    outpost.dhnt.io/affinity: user
{{- end }}
spec:
  selector:
    app: {{.Name}}
  ports:
  - port: 80
    targetPort: {{.Port}}
`
