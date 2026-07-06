// pxstress drives a configurable, multi-pool FADA-PVC stress test against the Portworx cluster
// behind px-smt-multirealm-shim. Each "pool" is a StatefulSet of single-node etcd instances (one FADA PVC per
// replica) in its own namespace. Pools cycle through a scale pattern — min → mid → max → mid,
// deleting the now-unused PVCs — and are periodically deleted and recreated wholesale. Every
// step verifies data integrity (a per-instance key written to etcd survives pod restarts and
// re-attaches) so any FADA data loss under churn is caught.
//
// It is intentionally dependency-free: it shells out to `oc` (or kubectl) using the caller's
// kubeconfig, so it builds to a static binary and runs from the jump host. Pools run
// concurrently in their own goroutines; on the first failure it either stops the whole run
// (stop-on-error) or records and continues.
//
//	pxstress -pools=3 -min=1 -max=5 -duration=1h -stop-on-error=true
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type opts struct {
	oc           string
	pools        int
	min          int
	mid          int
	max          int
	duration     time.Duration
	stepInterval time.Duration
	readyTimeout time.Duration
	recreateEach int // every N cycles a pool is fully deleted + recreated
	nsPrefix     string
	storageClass string
	size         string
	etcdImage    string
	stopOnError  bool
	cleanup      bool
	cmdTimeout   time.Duration
}

// cmdTimeout bounds each individual oc invocation. FADA volume detach under heavy concurrent
// churn can make `oc delete pod/namespace --wait` take minutes, so this is generous by default.
var cmdTimeout = 5 * time.Minute

func main() {
	o := opts{}
	flag.StringVar(&o.oc, "oc", "oc", "path to oc/kubectl")
	flag.IntVar(&o.pools, "pools", 3, "number of etcd pools (namespaces)")
	flag.IntVar(&o.min, "min", 1, "min instances per pool")
	flag.IntVar(&o.mid, "mid", 3, "mid instances per pool")
	flag.IntVar(&o.max, "max", 5, "max instances per pool")
	flag.DurationVar(&o.duration, "duration", time.Hour, "total run time")
	flag.DurationVar(&o.stepInterval, "step-interval", 20*time.Second, "settle time between scale steps")
	flag.DurationVar(&o.readyTimeout, "ready-timeout", 4*time.Minute, "per-scale readiness timeout")
	flag.IntVar(&o.recreateEach, "recreate-every", 2, "every Nth cycle, delete+recreate the pool (0=never)")
	flag.StringVar(&o.nsPrefix, "ns-prefix", "pxstress", "namespace prefix")
	flag.StringVar(&o.storageClass, "storageclass", "px-fada", "FADA StorageClass")
	flag.StringVar(&o.size, "size", "1Gi", "PVC size per instance")
	flag.StringVar(&o.etcdImage, "etcd-image", "quay.io/coreos/etcd:v3.5.17", "etcd image")
	flag.BoolVar(&o.stopOnError, "stop-on-error", true, "stop the whole run on the first failure")
	flag.BoolVar(&o.cleanup, "cleanup", true, "delete all pool namespaces at the end")
	flag.DurationVar(&o.cmdTimeout, "cmd-timeout", 5*time.Minute, "per-oc-command timeout (FADA detach can be slow)")
	flag.Parse()
	cmdTimeout = o.cmdTimeout

	logf("pxstress start pools=%d scale=%d/%d/%d duration=%s stopOnError=%v sc=%s image=%s",
		o.pools, o.min, o.mid, o.max, o.duration, o.stopOnError, o.storageClass, o.etcdImage)

	ctx, cancel := context.WithTimeout(context.Background(), o.duration)
	defer cancel()

	rec := &recorder{}
	var wg sync.WaitGroup
	for p := 0; p < o.pools; p++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			runPool(ctx, cancel, o, rec, idx)
		}(p)
	}
	wg.Wait()

	if o.cleanup {
		logf("cleanup: deleting %d pool namespaces", o.pools)
		for p := 0; p < o.pools; p++ {
			_, _ = run(context.Background(), o.oc, "delete", "namespace", nsName(o, p), "--ignore-not-found", "--wait=false")
		}
	}

	fails := rec.all()
	logf("pxstress done: %d failure(s)", len(fails))
	for _, f := range fails {
		logf("  FAIL %s", f)
	}
	if len(fails) > 0 {
		os.Exit(1)
	}
	logf("SUCCESS: no data loss or failures across the run")
}

// runPool loops one pool through the scale lifecycle until the context ends.
func runPool(ctx context.Context, cancel context.CancelFunc, o opts, rec *recorder, idx int) {
	ns := nsName(o, idx)
	pool := &poolState{o: o, ns: ns, idx: idx, rec: rec, expected: map[int]string{}}
	fail := func(step string, err error) bool {
		if err == nil {
			return false
		}
		// The run ending (duration elapsed, or another pool tripped stop-on-error) cancels the
		// context and surfaces as an error mid-operation — that is a clean stop, not a failure.
		if ctx.Err() != nil {
			return true
		}
		rec.add(fmt.Sprintf("[%s] %s: %v", ns, step, err))
		logf("[%s] FAILURE at %s: %v", ns, step, err)
		if o.stopOnError {
			cancel()
		}
		return true
	}

	if fail("create", pool.ensure(ctx, o.min)) {
		return
	}
	if fail("integrity(min)", pool.reconcileIntegrity(ctx, o.min)) {
		return
	}

	cycle := 0
	for ctx.Err() == nil {
		cycle++
		logf("[%s] cycle %d begin", ns, cycle)
		// Scale pattern min → mid → max → mid, verifying integrity at each stop.
		for _, target := range []int{o.mid, o.max, o.mid} {
			if ctx.Err() != nil {
				return
			}
			if fail(fmt.Sprintf("scale->%d", target), pool.scale(ctx, target)) {
				return
			}
			// On scale-down, delete the now-unused PVCs (ordinals >= target).
			if target < pool.current {
				if fail(fmt.Sprintf("pvc-cleanup>%d", target), pool.deletePVCsFrom(ctx, target)) {
					return
				}
			}
			pool.current = target
			if fail(fmt.Sprintf("integrity@%d", target), pool.reconcileIntegrity(ctx, target)) {
				return
			}
			sleepCtx(ctx, o.stepInterval)
		}
		// Failover/data-durability probe: kill instance 0 and confirm its key survives the
		// FADA detach+reattach — exactly the corruption the shim fix + cleaner guard against.
		if fail("kill-pod-0", pool.killAndVerify(ctx, 0)) {
			return
		}
		// Periodically delete + recreate the whole pool.
		if o.recreateEach > 0 && cycle%o.recreateEach == 0 {
			logf("[%s] cycle %d: full delete + recreate", ns, cycle)
			if fail("recreate", pool.recreate(ctx, o.min)) {
				return
			}
			if fail("integrity(recreated)", pool.reconcileIntegrity(ctx, o.min)) {
				return
			}
		}
		logf("[%s] cycle %d done", ns, cycle)
	}
}

type poolState struct {
	o        opts
	ns       string
	idx      int
	current  int
	rec      *recorder
	expected map[int]string // ordinal -> integrity value currently expected in that etcd
}

func (p *poolState) ensure(ctx context.Context, replicas int) error {
	manifest := renderManifest(p.o, p.ns, replicas)
	if _, err := runStdin(ctx, manifest, p.o.oc, "apply", "-f", "-"); err != nil {
		return err
	}
	p.current = replicas
	return p.waitReady(ctx, replicas)
}

func (p *poolState) scale(ctx context.Context, replicas int) error {
	if _, err := run(ctx, p.o.oc, "-n", p.ns, "scale", "statefulset", "etcd", "--replicas="+strconv.Itoa(replicas)); err != nil {
		return err
	}
	return p.waitReady(ctx, replicas)
}

func (p *poolState) recreate(ctx context.Context, replicas int) error {
	if _, err := run(ctx, p.o.oc, "delete", "namespace", p.ns, "--ignore-not-found", "--wait=true"); err != nil {
		return err
	}
	p.expected = map[int]string{}
	return p.ensure(ctx, replicas)
}

func (p *poolState) waitReady(ctx context.Context, replicas int) error {
	deadline := time.Now().Add(p.o.readyTimeout)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		out, err := run(ctx, p.o.oc, "-n", p.ns, "get", "statefulset", "etcd",
			"-o", "jsonpath={.status.readyReplicas}")
		if err == nil {
			ready, _ := strconv.Atoi(strings.TrimSpace(out))
			if ready == replicas {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for %d/%d ready replicas (last=%q)", replicas, replicas, strings.TrimSpace(out))
		}
		sleepCtx(ctx, 5*time.Second)
	}
}

// reconcileIntegrity ensures every running instance (ordinal < replicas) holds its expected key:
// a fresh instance (no expected value) gets one written; an existing one is read back and must
// match — a mismatch or missing value is data loss.
func (p *poolState) reconcileIntegrity(ctx context.Context, replicas int) error {
	for ord := 0; ord < replicas; ord++ {
		want, seen := p.expected[ord]
		if !seen {
			val := fmt.Sprintf("%s-ord%d-%d", p.ns, ord, ord*7+p.idx*100+1)
			if err := p.etcdPut(ctx, ord, val); err != nil {
				return fmt.Errorf("ordinal %d put: %w", ord, err)
			}
			p.expected[ord] = val
			continue
		}
		got, err := p.etcdGet(ctx, ord)
		if err != nil {
			return fmt.Errorf("ordinal %d get: %w", ord, err)
		}
		if got != want {
			return fmt.Errorf("ordinal %d DATA LOSS: want %q got %q", ord, want, got)
		}
	}
	return nil
}

// killAndVerify deletes one instance's pod (forcing a FADA detach + reattach) and confirms its
// key survives — the direct data-durability check for the LUN-churn / recycle bugs.
func (p *poolState) killAndVerify(ctx context.Context, ord int) error {
	if _, seen := p.expected[ord]; !seen {
		return nil
	}
	pod := fmt.Sprintf("etcd-%d", ord)
	if _, err := run(ctx, p.o.oc, "-n", p.ns, "delete", "pod", pod, "--wait=true"); err != nil {
		return err
	}
	if err := p.waitReady(ctx, p.current); err != nil {
		return err
	}
	got, err := p.etcdGet(ctx, ord)
	if err != nil {
		return fmt.Errorf("post-kill get: %w", err)
	}
	if got != p.expected[ord] {
		return fmt.Errorf("post-kill DATA LOSS on %s: want %q got %q", pod, p.expected[ord], got)
	}
	logf("[%s] kill+verify %s: key survived", p.ns, pod)
	return nil
}

func (p *poolState) deletePVCsFrom(ctx context.Context, keep int) error {
	// StatefulSet scale-down leaves PVCs behind; delete data-etcd-<ord> for ord >= keep.
	for ord := keep; ord <= p.o.max; ord++ {
		pvc := fmt.Sprintf("data-etcd-%d", ord)
		if _, err := run(ctx, p.o.oc, "-n", p.ns, "delete", "pvc", pvc, "--ignore-not-found", "--wait=false"); err != nil {
			return err
		}
		delete(p.expected, ord)
	}
	return nil
}

func (p *poolState) etcdPut(ctx context.Context, ord int, val string) error {
	pod := fmt.Sprintf("etcd-%d", ord)
	_, err := run(ctx, p.o.oc, "-n", p.ns, "exec", pod, "--", "etcdctl", "put", "integrity", val)
	return err
}

func (p *poolState) etcdGet(ctx context.Context, ord int) (string, error) {
	pod := fmt.Sprintf("etcd-%d", ord)
	out, err := run(ctx, p.o.oc, "-n", p.ns, "exec", pod, "--", "etcdctl", "get", "integrity", "--print-value-only")
	return strings.TrimSpace(out), err
}

// ---- helpers ----

func nsName(o opts, idx int) string { return fmt.Sprintf("%s-%d", o.nsPrefix, idx) }

type recorder struct {
	mu   sync.Mutex
	fail []string
}

func (r *recorder) add(s string) { r.mu.Lock(); r.fail = append(r.fail, s); r.mu.Unlock() }
func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.fail...)
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	// Detach from a cancelled parent for delete calls handled by callers passing Background.
	cmd := exec.CommandContext(cctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func runStdin(ctx context.Context, stdin string, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func logf(format string, a ...any) {
	fmt.Printf("%s "+format+"\n", append([]any{time.Now().UTC().Format("15:04:05")}, a...)...)
}

func renderManifest(o opts, ns string, replicas int) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %[1]s
  labels: {pxstress: "true"}
---
apiVersion: v1
kind: Service
metadata: {name: etcd, namespace: %[1]s}
spec:
  clusterIP: None
  selector: {app: etcd}
  ports: [{name: client, port: 2379}]
---
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: etcd, namespace: %[1]s}
spec:
  serviceName: etcd
  replicas: %[2]d
  selector: {matchLabels: {app: etcd}}
  template:
    metadata: {labels: {app: etcd}}
    spec:
      terminationGracePeriodSeconds: 5
      containers:
        - name: etcd
          image: %[3]s
          command: ["etcd"]
          args:
            - "--name=n"
            - "--data-dir=/data/etcd"
            - "--listen-client-urls=http://0.0.0.0:2379"
            - "--advertise-client-urls=http://127.0.0.1:2379"
            - "--listen-peer-urls=http://0.0.0.0:2380"
            - "--initial-advertise-peer-urls=http://127.0.0.1:2380"
            - "--initial-cluster=n=http://127.0.0.1:2380"
            - "--initial-cluster-state=new"
          env:
            - {name: ETCDCTL_API, value: "3"}
          volumeMounts: [{name: data, mountPath: /data}]
          readinessProbe:
            exec: {command: ["etcdctl", "endpoint", "health"]}
            initialDelaySeconds: 5
            periodSeconds: 5
            failureThreshold: 6
  volumeClaimTemplates:
    - metadata: {name: data}
      spec:
        accessModes: ["ReadWriteOnce"]
        storageClassName: %[4]s
        resources: {requests: {storage: %[5]s}}
`, ns, replicas, o.etcdImage, o.storageClass, o.size)
}
