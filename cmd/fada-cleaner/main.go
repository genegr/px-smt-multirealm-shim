// fada-cleaner is a per-node DaemonSet agent that closes the FADA LUN-recycle hole.
//
// Background: a FlashArray recycles LUN numbers on detach/reattach (like every Cinder
// driver). Portworx's CSI path leaves a stale multipath map + SCSI devices behind when it
// tears down a FADA volume whose on-node LUN it can't correctly reconcile (the shim's
// realm-host vs array-level-host LUN mismatch). When the array then re-hands that LUN number
// to a *different* volume, the stale device handle silently serves the new volume's blocks —
// data corruption. Proven cold: the same /dev/sdX that backed volA came back reporting volB's
// WWID, and reads through the old map no longer returned volA's data.
//
// The fix is the full host-side disconnect that px-csi omits: flush the stale multipath map and
// delete every backing SCSI device so the LUN is fully logged out and cannot be silently reused.
//
// v2 trigger + safety model:
//   - A Kubernetes VolumeAttachment watch gives the ground-truth *detach* signal for this node.
//     A DELETE means the volume is really leaving (not a pod restart, which keeps the
//     VolumeAttachment); we react immediately, eliminating the poll window.
//   - Every flush is gated on device-mapper open count 0 AND path-health: the map is only cleaned
//     if its backing SCSI paths are actually stale — all paths down, or a path whose SCSI id no
//     longer matches the map WWID (the LUN was reassigned). A healthy-but-briefly-unmounted map
//     (same-node pod restart) has running, matching paths and is never touched, regardless of how
//     long the restart takes.
//   - A periodic poll remains as a backstop for anything the watch misses, using the same gate
//     plus a short grace streak.
//
// All host interaction goes through nsenter into PID 1's namespaces, so the agent uses the
// node's own multipath/dmsetup/iscsiadm and sees the host's /sys and /dev. Requires a
// privileged, hostPID container. The watch uses the in-cluster ServiceAccount (stdlib only).
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type mpMap struct {
	WWID   string
	DM     string   // e.g. dm-9
	Vendor string   // e.g. "PURE,FlashArray"
	Paths  []string // backing sd device names, e.g. sdj
}

var (
	// Header line: "3624a9370... dm-9 PURE,FlashArray"  (user_friendly_names off → name == WWID)
	reHeader = regexp.MustCompile(`^([0-9a-fA-F]{20,})\s+(dm-\d+)\s+(\S+)`)
	// Path line: "|- 3:0:0:200 sdk 8:160 active ready running" / "`- 4:0:0:200 sdj ..."
	rePath = regexp.MustCompile(`\b\d+:\d+:\d+:\d+\s+(sd[a-z]+)\b`)
)

type config struct {
	pollEvery  time.Duration
	gracePolls int
	vendor     string
	dryRun     bool
	nodeName   string
}

func loadConfig() config {
	c := config{pollEvery: 8 * time.Second, gracePolls: 2, vendor: "PURE", dryRun: false, nodeName: os.Getenv("NODE_NAME")}
	if v := os.Getenv("FADA_POLL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.pollEvery = time.Duration(n) * time.Second
		}
	}
	if v := os.Getenv("FADA_GRACE_POLLS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			c.gracePolls = n
		}
	}
	if v := os.Getenv("FADA_VENDOR"); v != "" {
		c.vendor = v
	}
	if v := os.Getenv("FADA_DRY_RUN"); v == "true" || v == "1" {
		c.dryRun = true
	}
	return c
}

// hostCmd runs a command inside PID 1's namespaces so it executes the node's own tooling
// against the host's /sys and /dev. A timeout keeps a blocked device (e.g. scsi_id on a LUN
// that was yanked) from stalling the scan loop.
func hostCmd(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	full := append([]string{"-t", "1", "-m", "-u", "-i", "-n", "-p", "--"}, args...)
	out, err := exec.CommandContext(ctx, "nsenter", full...).CombinedOutput()
	return string(out), err
}

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stdout, "%s "+format+"\n", append([]any{time.Now().UTC().Format(time.RFC3339)}, a...)...)
}

func parseMultipath(out string) []mpMap {
	var maps []mpMap
	var cur *mpMap
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if m := reHeader.FindStringSubmatch(line); m != nil {
			if cur != nil {
				maps = append(maps, *cur)
			}
			cur = &mpMap{WWID: strings.ToLower(m[1]), DM: m[2], Vendor: m[3]}
			continue
		}
		if cur != nil {
			if p := rePath.FindStringSubmatch(line); p != nil {
				cur.Paths = append(cur.Paths, p[1])
			}
		}
	}
	if cur != nil {
		maps = append(maps, *cur)
	}
	return maps
}

// openCount returns the device-mapper open count for a map (mounts + higher dm layers + any
// process holding the device). -1 means "could not determine" → treat as in-use, never flush.
func openCount(name string) int {
	out, err := hostCmd("dmsetup", "info", "-c", "--noheadings", "-o", "open", name)
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return -1
	}
	return n
}

func pathState(dev string) string {
	out, err := hostCmd("cat", "/sys/block/"+dev+"/device/state")
	if err != nil {
		return "missing"
	}
	return strings.TrimSpace(out)
}

func scsiID(dev string) string {
	out, err := hostCmd("/lib/udev/scsi_id", "-g", "-u", "-d", "/dev/"+dev)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(out))
}

// pathsStale reports whether a map's backing paths are actually gone/reassigned — the signal
// that distinguishes a real detach from a healthy-but-unmounted volume (same-node pod restart).
// Only ever consulted for idle (open=0) maps, so a healthy path reliably answers a SCSI INQUIRY.
// A path counts as healthy only if its SCSI id still equals the map WWID *and* it is running; a
// detached LUN fails the INQUIRY (empty id) or has been reassigned (different id). Stale iff any
// path resolves to a different, non-empty WWID (the array recycled the LUN to another volume — a
// definite corruption risk), or no path remains healthy. A reason string is returned for logging.
func pathsStale(m mpMap) (bool, string) {
	if len(m.Paths) == 0 {
		return true, "no-paths"
	}
	anyHealthy := false
	for _, dev := range m.Paths {
		sid := scsiID(dev)
		if sid != "" && sid != m.WWID {
			return true, "scsi-id-mismatch:" + dev + "=" + sid // LUN reassigned to another volume
		}
		if sid == m.WWID && pathState(dev) == "running" {
			anyHealthy = true
		}
	}
	if !anyHealthy {
		return true, "no-healthy-path"
	}
	return false, ""
}

// fullDisconnect performs the teardown px-csi skips: flush the multipath map, then delete each
// backing SCSI device so the iSCSI LUN is fully logged out and cannot be silently reused.
func fullDisconnect(m mpMap, dryRun bool) {
	if dryRun {
		logf("DRYRUN would flush wwid=%s dm=%s paths=%v", m.WWID, m.DM, m.Paths)
		return
	}
	if out, err := hostCmd("multipath", "-f", m.WWID); err != nil {
		logf("WARN multipath -f wwid=%s err=%v out=%q", m.WWID, err, strings.TrimSpace(out))
	}
	for _, dev := range m.Paths {
		// echo 1 > /sys/block/<dev>/device/delete — issued in the host mount ns.
		if out, err := hostCmd("sh", "-c", "echo 1 > /sys/block/"+dev+"/device/delete"); err != nil {
			logf("WARN delete path=%s err=%v out=%q", dev, err, strings.TrimSpace(out))
		}
	}
	logf("FLUSHED wwid=%s dm=%s paths=%v", m.WWID, m.DM, m.Paths)
}

// scanAndClean runs one cleanup pass. trigger describes why (poll / volumeattachment-delete);
// on a detach event a stale map is flushed immediately, otherwise it must persist across the
// grace streak. orphanStreak is shared across passes and mutated here.
func scanAndClean(cfg config, orphanStreak map[string]int, trigger string) {
	out, err := hostCmd("multipath", "-ll")
	if err != nil && out == "" {
		logf("WARN multipath -ll err=%v", err)
		return
	}
	event := trigger != "poll"
	seen := map[string]bool{}
	for _, m := range parseMultipath(out) {
		if !strings.Contains(strings.ToUpper(m.Vendor), strings.ToUpper(cfg.vendor)) {
			continue // only our FlashArray devices
		}
		seen[m.WWID] = true
		if openCount(m.WWID) != 0 {
			orphanStreak[m.WWID] = 0 // in use (or indeterminate) → hands off
			continue
		}
		stale, reason := pathsStale(m)
		if !stale {
			// open=0 but paths healthy: a briefly-unmounted, still-attached volume. Never flush.
			orphanStreak[m.WWID] = 0
			continue
		}
		orphanStreak[m.WWID]++
		if event || orphanStreak[m.WWID] >= cfg.gracePolls {
			logf("STALE wwid=%s open=0 reason=%s trigger=%s streak=%d -> full disconnect",
				m.WWID, reason, trigger, orphanStreak[m.WWID])
			fullDisconnect(m, cfg.dryRun)
			delete(orphanStreak, m.WWID)
		}
	}
	for w := range orphanStreak {
		if !seen[w] {
			delete(orphanStreak, w)
		}
	}
}

// watchVolumeAttachments streams VolumeAttachment events from the API server using the
// in-cluster ServiceAccount and pokes trigger on every DELETE for this node. Stdlib only; it
// reconnects on stream end. If in-cluster credentials are absent, it returns and the agent runs
// poll-only.
func watchVolumeAttachments(cfg config, trigger chan<- struct{}) {
	const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	token, err := os.ReadFile(saDir + "/token")
	if err != nil {
		logf("watch disabled (no in-cluster token): %v — running poll-only", err)
		return
	}
	caPEM, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		logf("watch disabled (no ca.crt): %v — running poll-only", err)
		return
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		logf("watch disabled (no KUBERNETES_SERVICE_HOST) — running poll-only")
		return
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	url := fmt.Sprintf("https://%s:%s/apis/storage.k8s.io/v1/volumeattachments?watch=true&timeoutSeconds=300", host, port)

	for {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+string(token))
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			logf("watch connect err=%v; retry in 5s", err)
			time.Sleep(5 * time.Second)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			logf("watch http=%d; retry in 5s", resp.StatusCode)
			time.Sleep(5 * time.Second)
			continue
		}
		logf("watch connected to VolumeAttachments (node=%s)", cfg.nodeName)
		dec := json.NewDecoder(resp.Body)
		for {
			var ev struct {
				Type   string `json:"type"`
				Object struct {
					Spec struct {
						NodeName string `json:"nodeName"`
					} `json:"spec"`
				} `json:"object"`
			}
			if err := dec.Decode(&ev); err != nil {
				break // stream ended or errored → reconnect
			}
			if ev.Type == "DELETED" && (cfg.nodeName == "" || ev.Object.Spec.NodeName == cfg.nodeName) {
				logf("detach event: VolumeAttachment DELETED node=%s", ev.Object.Spec.NodeName)
				select {
				case trigger <- struct{}{}:
				default: // a scan is already pending; coalesce
				}
			}
		}
		resp.Body.Close()
	}
}

func main() {
	cfg := loadConfig()
	logf("fada-cleaner v2 start node=%q poll=%s grace=%d vendor=%q dryRun=%v",
		cfg.nodeName, cfg.pollEvery, cfg.gracePolls, cfg.vendor, cfg.dryRun)

	orphanStreak := map[string]int{}
	var mu sync.Mutex // scans never overlap
	trigger := make(chan struct{}, 1)

	go watchVolumeAttachments(cfg, trigger)

	ticker := time.NewTicker(cfg.pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			mu.Lock()
			scanAndClean(cfg, orphanStreak, "poll")
			mu.Unlock()
		case <-trigger:
			// Give the kernel/multipathd a moment to mark the just-detached paths failed.
			time.Sleep(2 * time.Second)
			mu.Lock()
			scanAndClean(cfg, orphanStreak, "volumeattachment-delete")
			mu.Unlock()
		}
	}
}
