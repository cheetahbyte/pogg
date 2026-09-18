package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

type containerInfo struct {
	Name   string
	Config struct {
		Image string
		Env   []string
	}
	State           struct{ Running bool }
	NetworkSettings struct {
		Ports map[string][]struct {
			HostIp   string
			HostPort string
		}
	}
}

func inspect(ctx context.Context, name string) (Instance, string, error) {
	b, e := exec.CommandContext(ctx, "docker", "inspect", "--type", "container", name).Output()
	if e != nil {
		return Instance{}, "", fmt.Errorf("cannot inspect container %q", name)
	}
	var items []containerInfo
	if e = json.Unmarshal(b, &items); e != nil {
		return Instance{}, "", e
	}
	if len(items) != 1 || !items[0].State.Running {
		return Instance{}, "", fmt.Errorf("container %q is not running", name)
	}
	c := items[0]
	env := map[string]string{}
	for _, v := range c.Config.Env {
		k, v, ok := strings.Cut(v, "=")
		if ok {
			env[k] = v
		}
	}
	ports := c.NetworkSettings.Ports["5432/tcp"]
	if len(ports) == 0 {
		return Instance{}, "", fmt.Errorf("container %q has no published PostgreSQL port", name)
	}
	host := ports[0].HostIp
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "::" {
		host = "::1"
	}
	port, e := strconv.Atoi(ports[0].HostPort)
	if e != nil || port < 1 || port > 65535 {
		return Instance{}, "", fmt.Errorf("invalid published port")
	}
	admin := env["POSTGRES_USER"]
	if admin == "" {
		admin = "postgres"
	}
	return Instance{Container: strings.TrimPrefix(c.Name, "/"), Host: host, Port: port, Admin: admin, SSLMode: "disable"}, env["POSTGRES_PASSWORD"], nil
}
func discover(ctx context.Context) ([]string, error) {
	b, e := exec.CommandContext(ctx, "docker", "ps", "--format", "{{.Names}}").Output()
	if e != nil {
		return nil, fmt.Errorf("Docker discovery unavailable; use instance add with --host, --port and --admin")
	}
	var names []string
	for _, n := range strings.Fields(string(b)) {
		i, _, e := inspect(ctx, n)
		if e == nil && i.Port != 0 {
			names = append(names, n)
		}
	}
	return names, nil
}
func (s *store) resolve(ctx context.Context, explicit, container, appInstance string) (string, error) {
	if explicit != "" && container != "" {
		return "", fmt.Errorf("choose --instance or --container, not both")
	}
	if container != "" {
		i, p, e := inspect(ctx, container)
		if e != nil {
			return "", e
		}
		for n, old := range s.state.Instances {
			if old.Container == i.Container {
				return n, nil
			}
		}
		name := i.Container
		if e = checkInstanceName(name); e != nil {
			return "", fmt.Errorf("container name cannot be used as an instance name; register it with instance add NAME --container CONTAINER")
		}
		if _, ok := s.state.Instances[name]; ok {
			return "", fmt.Errorf("instance %q already exists", name)
		}
		if e = s.secrets.Set(adminKey(name), p); e != nil {
			return "", e
		}
		s.state.Instances[name] = i
		if s.state.Default == "" {
			s.state.Default = name
		}
		return name, s.save()
	}
	name := explicit
	if name == "" {
		name = appInstance
	}
	if name == "" {
		name = s.state.Default
	}
	if name != "" {
		if _, ok := s.state.Instances[name]; !ok {
			return "", fmt.Errorf("unknown instance %q", name)
		}
		return name, nil
	}
	names, e := discover(ctx)
	if e != nil {
		return "", e
	}
	if len(names) != 1 {
		return "", fmt.Errorf("found %d PostgreSQL containers (%s); select one with --container or register instance add", len(names), strings.Join(names, ", "))
	}
	return s.resolve(ctx, "", names[0], "")
}
func connectionURL(i Instance, db, user, pass string) string {
	u := url.URL{Scheme: "postgresql", User: url.UserPassword(user, pass), Host: net.JoinHostPort(i.Host, strconv.Itoa(i.Port)), Path: "/" + db}
	q := url.Values{}
	q.Set("sslmode", i.SSLMode)
	u.RawQuery = q.Encode()
	return u.String()
}
func connect(ctx context.Context, i Instance, db, user, pass string) (*pgx.Conn, error) {
	cfg, e := pgx.ParseConfig(connectionURL(i, db, user, pass))
	if e != nil {
		return nil, fmt.Errorf("invalid connection configuration")
	}
	c, e := pgx.ConnectConfig(ctx, cfg)
	if e != nil {
		return nil, fmt.Errorf("connection to %s:%d/%s as %s failed (check credentials and availability)", i.Host, i.Port, db, user)
	}
	return c, nil
}
func (s *store) admin(ctx context.Context, name string) (*pgx.Conn, error) {
	i, ok := s.state.Instances[name]
	if !ok {
		return nil, fmt.Errorf("unknown instance %q", name)
	}
	p, e := s.secrets.Get(adminKey(name))
	if e != nil {
		return nil, e
	}
	if i.Container != "" {
		fresh, _, e := inspect(ctx, i.Container)
		if e != nil {
			return nil, e
		}
		// Keep registered credentials: container environment may no longer match ALTER ROLE.
		if i.Host != fresh.Host || i.Port != fresh.Port {
			i.Host = fresh.Host
			i.Port = fresh.Port
			s.state.Instances[name] = i
			if e = s.save(); e != nil {
				return nil, e
			}
		}
	}
	c, err := connect(ctx, i, "postgres", i.Admin, p)
	if err == nil || i.Container == "" {
		return c, err
	}
	// A recreated container may have new bootstrap credentials. Never change the server's password.
	fresh, detected, inspectErr := inspect(ctx, i.Container)
	if inspectErr != nil || (detected == p && fresh.Admin == i.Admin) {
		return nil, err
	}
	fresh.SSLMode = i.SSLMode
	c, err = connect(ctx, fresh, "postgres", fresh.Admin, detected)
	if err != nil {
		return nil, err
	}
	if err = s.secrets.Set(adminKey(name), detected); err != nil {
		c.Close(ctx)
		return nil, err
	}
	s.state.Instances[name] = fresh
	if err = s.save(); err != nil {
		c.Close(ctx)
		return nil, err
	}
	return c, nil
}

// Rediscovery changes only the default, never an existing app's instance binding.
func (s *store) rediscoverDefault(ctx context.Context) (string, error) {
	old, ok := s.state.Instances[s.state.Default]
	if !ok || old.Container == "" {
		return "", nil
	}
	b, err := exec.CommandContext(ctx, "docker", "ps", "-a", "--format", "{{.Names}}").Output()
	if err != nil {
		return "", nil
	} // Keep the useful connection error if Docker is unavailable.
	for _, name := range strings.Fields(string(b)) {
		if name == old.Container {
			return "", nil
		}
	}
	names, err := discover(ctx)
	if err != nil {
		return "", err
	}
	if len(names) != 1 {
		return "", fmt.Errorf("default container is gone; found %d candidates (%s), select with --container", len(names), strings.Join(names, ", "))
	}
	name, err := s.resolve(ctx, "", names[0], "")
	if err != nil {
		return "", err
	}
	s.state.Default = name
	if err = s.save(); err != nil {
		return "", err
	}
	return "default container disappeared; selected " + name + "; existing apps remain bound to their original instance", nil
}
