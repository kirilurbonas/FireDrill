// Package gc reaps orphaned FireDrill sandboxes — containers, pods,
// ephemeral namespaces and Velero restore CRs left behind when a drill
// process died before its deferred teardown (crash, SIGKILL, node loss).
// The in-process TTL watchdog cannot outlive the process; this can.
package gc

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/moby/moby/client"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	sbk8s "github.com/kirilurbonas/FireDrill/pkg/sandbox/kubernetes"
)

// ExpiresAtLabel marks sandbox resources with their hard deadline so GC can
// distinguish live drills from orphans without guessing.
const ExpiresAtLabel = "firedrill.expires-at"

// Result summarizes one sweep.
type Result struct {
	Reaped  []string
	Skipped []string // live (not yet expired) sandboxes
	Errors  []string
}

// Options tune a sweep.
type Options struct {
	OlderThan time.Duration // age threshold for resources without an expiry label
	DryRun    bool
	// Force ignores the expiry label and reaps anything older than
	// OlderThan — for operators who know the owning process is dead.
	Force bool
}

// expired reports whether a resource's expiry label/annotation has passed.
// Resources without the label fall back to the age threshold. Reaping
// inside a live TTL lease requires Force: the owning process may be alive.
func (o Options) expired(expiresAt string, created time.Time, now time.Time) bool {
	if expiresAt != "" && !o.Force {
		if t, err := time.Parse(time.RFC3339, expiresAt); err == nil {
			return now.After(t)
		}
	}
	return now.Sub(created) > o.OlderThan
}

// SweepDocker reaps expired firedrill sandbox containers and networks.
func SweepDocker(ctx context.Context, o Options) (*Result, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("connecting to docker: %w", err)
	}
	if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
		return nil, fmt.Errorf("cannot reach the Docker daemon — is Docker running? (%w)", err)
	}
	res := &Result{}
	now := time.Now()

	containers, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	for _, c := range containers.Items {
		if c.Labels["firedrill"] != "sandbox" {
			continue
		}
		name := strings.TrimPrefix(firstName(c.Names), "/")
		if !o.expired(c.Labels[ExpiresAtLabel], time.Unix(c.Created, 0), now) {
			res.Skipped = append(res.Skipped, "container "+name)
			continue
		}
		if o.DryRun {
			res.Reaped = append(res.Reaped, "container "+name+" (dry-run)")
			continue
		}
		if _, err := cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("container %s: %v", name, err))
			continue
		}
		res.Reaped = append(res.Reaped, "container "+name)
	}

	networks, err := cli.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		return res, fmt.Errorf("listing networks: %w", err)
	}
	for _, nw := range networks.Items {
		if !strings.HasPrefix(nw.Name, "firedrill-") {
			continue
		}
		if now.Sub(nw.Created) <= o.OlderThan && !o.Force {
			res.Skipped = append(res.Skipped, "network "+nw.Name)
			continue
		}
		if o.DryRun {
			res.Reaped = append(res.Reaped, "network "+nw.Name+" (dry-run)")
			continue
		}
		if _, err := cli.NetworkRemove(ctx, nw.ID, client.NetworkRemoveOptions{}); err != nil {
			// Networks still attached to a live container fail — that's a skip.
			res.Skipped = append(res.Skipped, "network "+nw.Name+" (in use)")
			continue
		}
		res.Reaped = append(res.Reaped, "network "+nw.Name)
	}
	return res, nil
}

var restoreGVR = schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "restores"}

// SweepKubernetes reaps expired sandbox pods, ephemeral firedrill-*
// namespaces and firedrill-labeled Velero restore CRs.
func SweepKubernetes(ctx context.Context, o Options) (*Result, error) {
	restCfg, _, err := sbk8s.LoadConfig()
	if err != nil {
		return nil, err
	}
	cli, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, err
	}
	res := &Result{}
	now := time.Now()
	zero := int64(0)

	// Sandbox pods (shared namespace model).
	pods, err := cli.CoreV1().Pods("").List(ctx, metav1.ListOptions{LabelSelector: "firedrill/sandbox=true"})
	if err != nil {
		return nil, fmt.Errorf("listing sandbox pods: %w", err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		id := fmt.Sprintf("pod %s/%s", p.Namespace, p.Name)
		if !o.expired(p.Annotations[ExpiresAtLabel], p.CreationTimestamp.Time, now) {
			res.Skipped = append(res.Skipped, id)
			continue
		}
		if o.DryRun {
			res.Reaped = append(res.Reaped, id+" (dry-run)")
			continue
		}
		if err := cli.CoreV1().Pods(p.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{GracePeriodSeconds: &zero}); err != nil && !apierrors.IsNotFound(err) {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		res.Reaped = append(res.Reaped, id)
	}

	// Ephemeral velero-drill namespaces.
	nss, err := cli.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: "firedrill/sandbox=true"})
	if err != nil {
		return res, fmt.Errorf("listing namespaces: %w", err)
	}
	for i := range nss.Items {
		ns := &nss.Items[i]
		id := "namespace " + ns.Name
		if ns.Status.Phase == "Terminating" {
			res.Skipped = append(res.Skipped, id+" (terminating)")
			continue
		}
		if !o.expired(ns.Annotations[ExpiresAtLabel], ns.CreationTimestamp.Time, now) {
			res.Skipped = append(res.Skipped, id)
			continue
		}
		if o.DryRun {
			res.Reaped = append(res.Reaped, id+" (dry-run)")
			continue
		}
		if err := cli.CoreV1().Namespaces().Delete(ctx, ns.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		res.Reaped = append(res.Reaped, id)
	}

	// Leftover Velero restore CRs (only if the CRD exists).
	dyn, err := dynamic.NewForConfig(restCfg)
	if err == nil {
		restores, err := dyn.Resource(restoreGVR).Namespace("velero").List(ctx,
			metav1.ListOptions{LabelSelector: "app.kubernetes.io/managed-by=firedrill"})
		if err == nil {
			for _, r := range restores.Items {
				id := "restore " + r.GetName()
				if now.Sub(r.GetCreationTimestamp().Time) <= o.OlderThan && !o.Force {
					res.Skipped = append(res.Skipped, id)
					continue
				}
				if o.DryRun {
					res.Reaped = append(res.Reaped, id+" (dry-run)")
					continue
				}
				if err := dyn.Resource(restoreGVR).Namespace("velero").Delete(ctx, r.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
					res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", id, err))
					continue
				}
				res.Reaped = append(res.Reaped, id)
			}
		}
	}
	return res, nil
}

func firstName(names []string) string {
	if len(names) == 0 {
		return "?"
	}
	return names[0]
}
