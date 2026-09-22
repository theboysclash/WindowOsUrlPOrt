// Package vm supervises a single QEMU guest: it builds the command line,
// launches the process, negotiates QMP, sets a per-boot VNC password and
// exposes power and snapshot operations.
package vm

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/theboysclash/WindowOsUrlPOrt/internal/config"
)

type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateStopping State = "stopping"
	StateCrashed  State = "crashed"
)

var ErrNotRunning = errors.New("vm is not running")
var ErrAlreadyRunning = errors.New("vm is already running")

// Status is a snapshot of the manager for the API/UI.
type Status struct {
	State       State     `json:"state"`
	Accel       string    `json:"accel"`
	AccelNote   string    `json:"accel_note,omitempty"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	DiskExists  bool      `json:"disk_exists"`
	QEMUFound   bool      `json:"qemu_found"`
	QEMUPath    string    `json:"qemu_path,omitempty"`
	VNCPort     int       `json:"vnc_port"`
	Log         []string  `json:"log"`
	RAMMB       int       `json:"ram_mb"`
	CPUs        int       `json:"cpus"`
	ISOAttached bool      `json:"iso_attached"`
}

type Manager struct {
	cfg     config.VM
	baseDir string
	log     *slog.Logger

	mu          sync.Mutex
	state       State
	cmd         *exec.Cmd
	qmp         *QMP
	vncPassword string
	accelUsed   string
	accelNote   string
	startedAt   time.Time
	lastErr     error
	stopping    bool
	logRing     *ring
	exited      chan struct{}
}

func NewManager(cfg config.VM, baseDir string, log *slog.Logger) *Manager {
	return &Manager{
		cfg:     cfg,
		baseDir: baseDir,
		log:     log,
		state:   StateStopped,
		logRing: newRing(300),
	}
}

func (m *Manager) qemuBinary(name string) (string, error) {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	var candidates []string
	if m.cfg.QEMUDir != "" {
		candidates = append(candidates, filepath.Join(m.cfg.QEMUDir, name))
	}
	candidates = append(candidates, filepath.Join(m.baseDir, "third_party", "qemu", name), filepath.Join(m.baseDir, "qemu", name))
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	if runtime.GOOS == "windows" {
		for _, c := range []string{`C:\Program Files\qemu\` + name, `C:\Program Files (x86)\qemu\` + name} {
			if _, err := os.Stat(c); err == nil {
				return c, nil
			}
		}
	}
	return "", fmt.Errorf("%s not found: set vm.qemu_dir in config.yaml or place QEMU in %s", name, filepath.Join(m.baseDir, "third_party", "qemu"))
}

func defaultAccel() string {
	switch runtime.GOOS {
	case "windows":
		return "whpx"
	case "darwin":
		return "hvf"
	case "linux":
		return "kvm"
	}
	return "tcg"
}

func accelArg(accel string) string {
	switch accel {
	case "whpx":
		// kernel-irqchip=off avoids a known Windows guest hang on some hosts.
		return "whpx,kernel-irqchip=off"
	case "tcg":
		return "tcg,thread=multi"
	}
	return accel
}

func randomVNCPassword() (string, error) {
	// VNC auth only uses the first 8 characters of the password.
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b), nil
}

// Args builds the QEMU command line for the given accelerator.
func (m *Manager) Args(accel string) []string {
	cfg := m.cfg
	cpu := "host"
	if accel == "tcg" {
		cpu = "max"
	}
	args := []string{
		"-accel", accelArg(accel),
		"-machine", "q35",
		"-cpu", cpu,
		"-m", fmt.Sprint(cfg.RAMMB),
		"-smp", fmt.Sprint(cfg.CPUs),
		"-rtc", "base=localtime,clock=host",
		"-display", "none",
		"-vga", "std",
		"-usb", "-device", "usb-tablet",
		"-device", "e1000,netdev=n0",
		"-netdev", "user,id=n0",
		"-vnc", fmt.Sprintf("127.0.0.1:%d,password=on", cfg.VNCPort-5900),
		"-qmp", fmt.Sprintf("tcp:127.0.0.1:%d,server=on,wait=off", cfg.QMPPort),
		"-name", "vmserver-guest",
		// Clipboard sync between browser and guest; needs spice-guest-tools
		// (vdagent) installed in Windows, harmless otherwise.
		"-chardev", "qemu-vdagent,id=vdagent,name=vdagent,clipboard=on",
		"-device", "virtio-serial-pci",
		"-device", "virtserialport,chardev=vdagent,name=com.redhat.spice.0",
	}
	if runtime.GOOS == "windows" && accel == "whpx" {
		// Hyper-V enlightenments make Windows guests noticeably smoother.
		args[5] = "host,hv_relaxed,hv_spinlocks=0x1fff,hv_vapic,hv_time"
	}

	switch cfg.DiskInterface {
	case "virtio":
		args = append(args, "-drive", fmt.Sprintf("file=%s,if=virtio,format=qcow2,cache=writeback,discard=unmap", cfg.DiskPath))
	default:
		args = append(args,
			"-device", "ahci,id=ahci0",
			"-drive", fmt.Sprintf("file=%s,if=none,id=disk0,format=qcow2,cache=writeback,discard=unmap", cfg.DiskPath),
			"-device", "ide-hd,drive=disk0,bus=ahci0.0",
		)
	}

	bootOrder := "c"
	if cfg.ISOPath != "" {
		args = append(args, "-drive", fmt.Sprintf("file=%s,media=cdrom,readonly=on", cfg.ISOPath))
		bootOrder = "dc"
	}
	if cfg.VirtioISOPath != "" {
		args = append(args, "-drive", fmt.Sprintf("file=%s,media=cdrom,readonly=on", cfg.VirtioISOPath))
	}
	args = append(args, "-boot", "order="+bootOrder+",menu=on")
	args = append(args, cfg.ExtraArgs...)
	return args
}

// Start boots the guest. With accel "auto" the platform accelerator is tried
// first and TCG is used as a fallback if QEMU exits during early startup.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.state == StateRunning || m.state == StateStarting {
		m.mu.Unlock()
		return ErrAlreadyRunning
	}
	m.state = StateStarting
	m.lastErr = nil
	m.stopping = false
	m.accelNote = ""
	m.mu.Unlock()

	fail := func(err error) error {
		m.mu.Lock()
		m.state = StateStopped
		m.lastErr = err
		m.mu.Unlock()
		return err
	}

	if _, err := os.Stat(m.cfg.DiskPath); err != nil {
		return fail(fmt.Errorf("disk image %s not found: create it with `qemu-img create -f qcow2 %s 40G` or run the setup wizard", m.cfg.DiskPath, m.cfg.DiskPath))
	}
	bin, err := m.qemuBinary("qemu-system-x86_64")
	if err != nil {
		return fail(err)
	}

	accels := []string{m.cfg.Accel}
	if m.cfg.Accel == "auto" {
		accels = []string{defaultAccel(), "tcg"}
	}

	var lastErr error
	for i, accel := range accels {
		err := m.launch(ctx, bin, accel)
		if err == nil {
			if i > 0 {
				m.mu.Lock()
				m.accelNote = fmt.Sprintf("%s unavailable, running under software emulation (slow). See scripts/enable-whpx.ps1.", accels[0])
				m.mu.Unlock()
				m.log.Warn("hardware acceleration unavailable, using TCG", "tried", accels[0])
			}
			return nil
		}
		lastErr = err
		m.log.Warn("qemu failed to start", "accel", accel, "err", err)
	}
	return fail(lastErr)
}

// launch starts QEMU with one accelerator and waits until QMP is reachable
// and the VNC password is set. An early exit is reported as an error so the
// caller can try another accelerator.
func (m *Manager) launch(ctx context.Context, bin, accel string) error {
	args := m.Args(accel)
	m.log.Info("starting qemu", "bin", bin, "accel", accel)
	m.logRing.reset()
	m.logRing.add("$ " + bin + " " + strings.Join(args, " "))

	cmd := exec.Command(bin, args...)
	cmd.Dir = filepath.Dir(bin)
	cmd.Stdout = m.logRing
	cmd.Stderr = m.logRing
	hideConsoleWindow(cmd)

	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan struct{})
	go func() {
		err := cmd.Wait()
		m.onExit(cmd, err)
		close(exited)
	}()

	dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	qmpDone := make(chan error, 1)
	var q *QMP
	go func() {
		var err error
		q, err = DialQMP(dialCtx, fmt.Sprintf("127.0.0.1:%d", m.cfg.QMPPort))
		qmpDone <- err
	}()

	select {
	case <-exited:
		cancel()
		<-qmpDone
		return fmt.Errorf("qemu exited during startup: %s", m.logRing.tail(6))
	case err := <-qmpDone:
		if err != nil {
			_ = cmd.Process.Kill()
			<-exited
			return err
		}
	}

	pw, err := randomVNCPassword()
	if err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	if _, err := q.Execute(ctx, "set_password", map[string]any{"protocol": "vnc", "password": pw}); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("set vnc password: %w", err)
	}

	m.mu.Lock()
	m.cmd = cmd
	m.qmp = q
	m.vncPassword = pw
	m.accelUsed = accel
	m.startedAt = time.Now()
	m.state = StateRunning
	m.exited = exited
	m.mu.Unlock()

	go m.eventLoop(q)
	m.log.Info("qemu running", "accel", accel, "vnc", m.cfg.VNCPort)
	return nil
}

func (m *Manager) onExit(cmd *exec.Cmd, err error) {
	m.mu.Lock()
	if m.cmd != cmd && m.cmd != nil {
		m.mu.Unlock()
		return
	}
	intentional := m.stopping
	if m.qmp != nil {
		go m.qmp.Close()
	}
	m.cmd, m.qmp, m.vncPassword = nil, nil, ""
	if intentional || (err == nil) {
		m.state = StateStopped
	} else {
		m.state = StateCrashed
		m.lastErr = fmt.Errorf("qemu exited unexpectedly: %v; %s", err, m.logRing.tail(4))
	}
	restart := !intentional && m.cfg.RestartOnCrash && m.state == StateCrashed
	m.mu.Unlock()
	m.log.Info("qemu exited", "err", err, "intentional", intentional)

	if restart {
		time.Sleep(3 * time.Second)
		if err := m.Start(context.Background()); err != nil {
			m.log.Error("restart after crash failed", "err", err)
		}
	}
}

func (m *Manager) eventLoop(q *QMP) {
	for ev := range q.Events {
		m.logRing.add("[event] " + ev.Event)
		if ev.Event == "SHUTDOWN" || ev.Event == "POWERDOWN" {
			m.log.Info("guest event", "event", ev.Event)
		}
	}
}

// VNCCredentials returns the loopback address and password for the running
// guest's VNC server.
func (m *Manager) VNCCredentials() (addr, password string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != StateRunning {
		return "", "", ErrNotRunning
	}
	return fmt.Sprintf("127.0.0.1:%d", m.cfg.VNCPort), m.vncPassword, nil
}

func (m *Manager) qmpClient() (*QMP, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != StateRunning || m.qmp == nil {
		return nil, ErrNotRunning
	}
	return m.qmp, nil
}

// Shutdown sends an ACPI power button press; Windows shuts down cleanly if a
// user is logged in or the power policy allows it.
func (m *Manager) Shutdown(ctx context.Context) error {
	q, err := m.qmpClient()
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.stopping = true
	m.state = StateStopping
	m.mu.Unlock()
	_, err = q.Execute(ctx, "system_powerdown", nil)
	return err
}

// Reset performs a hard reset of the guest.
func (m *Manager) Reset(ctx context.Context) error {
	q, err := m.qmpClient()
	if err != nil {
		return err
	}
	_, err = q.Execute(ctx, "system_reset", nil)
	return err
}

// Pause and Resume freeze/unfreeze guest CPUs.
func (m *Manager) Pause(ctx context.Context) error {
	q, err := m.qmpClient()
	if err != nil {
		return err
	}
	_, err = q.Execute(ctx, "stop", nil)
	return err
}

func (m *Manager) Resume(ctx context.Context) error {
	q, err := m.qmpClient()
	if err != nil {
		return err
	}
	_, err = q.Execute(ctx, "cont", nil)
	return err
}

// ForceStop kills QEMU immediately (equivalent to pulling the power cord).
func (m *Manager) ForceStop() error {
	m.mu.Lock()
	cmd := m.cmd
	exited := m.exited
	if cmd == nil {
		m.mu.Unlock()
		return ErrNotRunning
	}
	m.stopping = true
	m.state = StateStopping
	m.mu.Unlock()
	if err := cmd.Process.Kill(); err != nil {
		return err
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
	}
	return nil
}

// Stop asks the guest to power off and force-kills it if it is still running
// after the grace period.
func (m *Manager) Stop(ctx context.Context, grace time.Duration) error {
	m.mu.Lock()
	exited := m.exited
	running := m.state == StateRunning
	m.mu.Unlock()
	if !running {
		return nil
	}
	if err := m.Shutdown(ctx); err != nil {
		return m.ForceStop()
	}
	select {
	case <-exited:
		return nil
	case <-time.After(grace):
		m.log.Warn("guest did not power off in time, killing")
		return m.ForceStop()
	}
}

// Snapshot operations use HMP savevm/loadvm on the qcow2 image.
func (m *Manager) SaveSnapshot(ctx context.Context, name string) error {
	if err := validateSnapshotName(name); err != nil {
		return err
	}
	q, err := m.qmpClient()
	if err != nil {
		return err
	}
	out, err := q.HMP(ctx, "savevm "+name)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		return errors.New(strings.TrimSpace(out))
	}
	return nil
}

func (m *Manager) LoadSnapshot(ctx context.Context, name string) error {
	if err := validateSnapshotName(name); err != nil {
		return err
	}
	q, err := m.qmpClient()
	if err != nil {
		return err
	}
	out, err := q.HMP(ctx, "loadvm "+name)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		return errors.New(strings.TrimSpace(out))
	}
	return nil
}

func (m *Manager) DeleteSnapshot(ctx context.Context, name string) error {
	if err := validateSnapshotName(name); err != nil {
		return err
	}
	q, err := m.qmpClient()
	if err != nil {
		return err
	}
	out, err := q.HMP(ctx, "delvm "+name)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		return errors.New(strings.TrimSpace(out))
	}
	return nil
}

type SnapshotInfo struct {
	ID   string `json:"id"`
	Tag  string `json:"tag"`
	Size string `json:"size"`
	Date string `json:"date"`
}

// ListSnapshots parses `info snapshots` HMP output.
func (m *Manager) ListSnapshots(ctx context.Context) ([]SnapshotInfo, error) {
	q, err := m.qmpClient()
	if err != nil {
		return nil, err
	}
	out, err := q.HMP(ctx, "info snapshots")
	if err != nil {
		return nil, err
	}
	var list []SnapshotInfo
	header := true
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "List of snapshots") || strings.HasPrefix(line, "There is no") {
			continue
		}
		if header && strings.HasPrefix(line, "ID") {
			header = false
			continue
		}
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		info := SnapshotInfo{ID: f[0], Tag: f[1], Size: f[2]}
		if len(f) >= 5 {
			info.Date = f[3] + " " + f[4]
		}
		list = append(list, info)
	}
	return list, nil
}

func validateSnapshotName(name string) error {
	if name == "" || len(name) > 64 {
		return errors.New("snapshot name must be 1-64 characters")
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return errors.New("snapshot name may only contain letters, digits, - and _")
		}
	}
	return nil
}

// CreateDisk creates an empty qcow2 image using qemu-img.
func (m *Manager) CreateDisk(ctx context.Context, sizeGB int) error {
	if sizeGB < 8 {
		return errors.New("disk must be at least 8 GB")
	}
	if _, err := os.Stat(m.cfg.DiskPath); err == nil {
		return fmt.Errorf("%s already exists", m.cfg.DiskPath)
	}
	img, err := m.qemuBinary("qemu-img")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.cfg.DiskPath), 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, img, "create", "-f", "qcow2", m.cfg.DiskPath, fmt.Sprintf("%dG", sizeGB))
	hideConsoleWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// UpdateConfig swaps the VM configuration used for the next boot.
func (m *Manager) UpdateConfig(cfg config.VM) {
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
}

func (m *Manager) Config() config.VM {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{
		State:       m.state,
		Accel:       m.accelUsed,
		AccelNote:   m.accelNote,
		StartedAt:   m.startedAt,
		VNCPort:     m.cfg.VNCPort,
		Log:         m.logRing.lines(),
		RAMMB:       m.cfg.RAMMB,
		CPUs:        m.cfg.CPUs,
		ISOAttached: m.cfg.ISOPath != "",
	}
	if m.lastErr != nil {
		st.LastError = m.lastErr.Error()
	}
	if _, err := os.Stat(m.cfg.DiskPath); err == nil {
		st.DiskExists = true
	}
	if p, err := m.qemuBinary("qemu-system-x86_64"); err == nil {
		st.QEMUFound = true
		st.QEMUPath = p
	}
	return st
}
