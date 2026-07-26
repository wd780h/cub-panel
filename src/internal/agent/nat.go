// Host-side DNAT for instances on unmanaged bridges.
//
// On an Incus-managed bridge the proxy devices run with nat=true, i.e. real
// kernel DNAT, and the guest sees the client's true source address. Foreign
// bridges (docker0, br0…) cannot use that mode, and the userspace proxy
// fallback rewrites the source to the host. To keep source addresses real
// there too, the agent installs its own iptables DNAT rules, tagged with a
// per-instance comment so they can be found and withdrawn again. The mapping
// is also recorded on the instance (user.cub-panel.dnat) so rules survive a
// host reboot: RestoreDNAT replays them at agent startup.
package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// hasIptables reports whether the host can take DNAT rules at all.
func hasIptables() bool {
	_, err := exec.LookPath("iptables")
	return err == nil
}

// dnatSpec is the serialised form stored in user.cub-panel.dnat.
type dnatSpec struct {
	Addr     string
	SSHPort  int
	PortFrom int
	PortTo   int
}

func (d dnatSpec) String() string {
	return fmt.Sprintf("%s|%d|%d|%d", d.Addr, d.SSHPort, d.PortFrom, d.PortTo)
}

func parseDNATSpec(s string) (dnatSpec, bool) {
	var d dnatSpec
	if s == "" {
		return d, false
	}
	n, err := fmt.Sscanf(strings.ReplaceAll(s, "|", " "), "%s %d %d %d",
		&d.Addr, &d.SSHPort, &d.PortFrom, &d.PortTo)
	return d, err == nil && n == 4 && ipv4Re.MatchString(d.Addr)
}

// comment tags every rule belonging to one instance.
func dnatComment(name string) string { return "cub-panel-" + name }

// ipt runs one iptables invocation.
func ipt(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "iptables", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// applyDNAT installs the instance's forwarding rules (idempotently).
func (s *Server) applyDNAT(ctx context.Context, name string, d dnatSpec) error {
	s.removeDNAT(ctx, name)
	c := dnatComment(name)
	add := func(rule ...string) error {
		args := append([]string{"-t", "nat", "-A", "PREROUTING"}, rule...)
		args = append(args, "-m", "comment", "--comment", c)
		return ipt(ctx, args...)
	}
	if d.SSHPort > 0 {
		if err := add("-p", "tcp", "--dport", fmt.Sprint(d.SSHPort),
			"-j", "DNAT", "--to-destination", d.Addr+":22"); err != nil {
			return err
		}
	}
	if d.PortFrom > 0 && d.PortTo >= d.PortFrom {
		rng := fmt.Sprintf("%d:%d", d.PortFrom, d.PortTo)
		for _, proto := range []string{"tcp", "udp"} {
			if err := add("-p", proto, "--dport", rng,
				"-j", "DNAT", "--to-destination", d.Addr); err != nil {
				return err
			}
		}
	}
	return nil
}

// removeDNAT withdraws every rule tagged for the instance.
func (s *Server) removeDNAT(ctx context.Context, name string) {
	out, err := exec.CommandContext(ctx, "iptables-save", "-t", "nat").Output()
	if err != nil {
		return
	}
	needle := `--comment ` + dnatComment(name)
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "-A PREROUTING") || !strings.Contains(line, needle) {
			continue
		}
		args := append([]string{"-t", "nat", "-D"}, strings.Fields(line)[1:]...)
		if err := ipt(ctx, args...); err != nil {
			s.log("dnat cleanup %s: %v", name, err)
		}
	}
}

// RestoreDNAT replays recorded mappings after a restart. Rules live in the
// kernel, so a host reboot silently drops them; the instance config is the
// durable copy.
func (s *Server) RestoreDNAT() {
	if !s.hasIPT {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Wait for the daemon to come up.
	for i := 0; i < 60; i++ {
		if _, err := s.lxd.Info(ctx); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
	names, err := s.lxd.List(ctx)
	if err != nil {
		s.log("dnat restore: list: %v", err)
		return
	}
	for _, name := range names {
		in, err := s.lxd.Get(ctx, name)
		if err != nil {
			continue
		}
		spec, ok := parseDNATSpec(in.Config["user.cub-panel.dnat"])
		if !ok {
			continue
		}
		if err := s.applyDNAT(ctx, name, spec); err != nil {
			s.log("dnat restore %s: %v", name, err)
		} else {
			s.log("dnat restored for %s", name)
		}
	}
}
