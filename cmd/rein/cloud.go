package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jordandalton/rein/internal/spec"
)

const defaultControlURL = "https://reincontrol.com"

type cloudProfile struct {
	ControlURL   string    `json:"control_url"`
	Organization string    `json:"organization"`
	TeamSlug     string    `json:"team_slug"`
	User         string    `json:"user"`
	DeviceID     string    `json:"device_id"`
	DeviceName   string    `json:"device_name"`
	LastContact  time.Time `json:"last_contact"`
}

type cloudIdentity struct {
	Organization string `json:"organization"`
	TeamSlug     string `json:"team_slug"`
	User         string `json:"user"`
	DeviceID     string `json:"device_id"`
	DeviceName   string `json:"device_name"`
}

type cloudAgent struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
}
type cloudAgents struct {
	Agents []cloudAgent `json:"agents"`
}

var (
	openBrowser     = openCloudBrowser
	cloudHTTPClient = &http.Client{Timeout: 15 * time.Second}
)

func cloudURL(override string) string {
	if override != "" {
		return strings.TrimRight(override, "/")
	}
	if configured := os.Getenv("REIN_CONTROL_URL"); configured != "" {
		return strings.TrimRight(configured, "/")
	}
	return defaultControlURL
}

var cloudProfileOverride string

func cloudProfilePath() string {
	name := cloudProfileOverride
	if name == "" {
		if b, err := os.ReadFile(filepath.Join(spec.Home(), "active-profile")); err == nil {
			name = strings.TrimSpace(string(b))
		}
	}
	if name != "" {
		return filepath.Join(spec.Home(), "cloud-"+name+".json")
	}
	return filepath.Join(spec.Home(), "cloud.json")
}
func cloudAgentsPath() string { return filepath.Join(spec.Home(), "agents.json") }
func cloudPolicyPath() string { return filepath.Join(spec.Home(), "policy.json") }

func loadAgents() (cloudAgents, error) {
	var a cloudAgents
	b, err := os.ReadFile(cloudAgentsPath())
	if os.IsNotExist(err) {
		return a, nil
	}
	if err != nil {
		return a, err
	}
	return a, json.Unmarshal(b, &a)
}
func saveAgents(a cloudAgents) error {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cloudAgentsPath(), append(b, '\n'), 0o600)
}

func loadCloudProfile() (*cloudProfile, error) {
	b, err := os.ReadFile(cloudProfilePath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var profile cloudProfile
	if err := json.Unmarshal(b, &profile); err != nil {
		return nil, fmt.Errorf("cloud profile is corrupt: %w", err)
	}
	return &profile, nil
}

func saveCloudProfile(profile cloudProfile) error {
	if err := os.MkdirAll(filepath.Dir(cloudProfilePath()), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cloudProfilePath(), append(b, '\n'), 0o600)
}

func cmdLogin(ctx context.Context, args []string) error {
	fs := flagSet("login")
	controlURL := fs.String("control-url", "", "")
	deviceName := fs.String("device-name", hostname(), "")
	profileName := fs.String("profile", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: rein login [--control-url URL] [--device-name NAME]")
	}
	cloudProfileOverride = strings.TrimSpace(*profileName)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("start login callback: %w", err)
	}
	defer listener.Close()
	state, err := randomState()
	if err != nil {
		return err
	}
	callback := "http://" + listener.Addr().String() + "/callback"
	loginURL, err := url.Parse(cloudURL(*controlURL) + "/rein/login")
	if err != nil {
		return err
	}
	q := loginURL.Query()
	q.Set("redirect_uri", callback)
	q.Set("state", state)
	q.Set("device_name", *deviceName)
	loginURL.RawQuery = q.Encode()

	type result struct {
		identity cloudIdentity
		token    string
		err      error
	}
	done := make(chan result, 1)
	var once sync.Once
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" || r.URL.Query().Get("state") != state {
			http.Error(w, "Invalid Rein login callback.", http.StatusBadRequest)
			return
		}
		identity := cloudIdentity{
			Organization: r.URL.Query().Get("organization"), TeamSlug: r.URL.Query().Get("team_slug"), User: r.URL.Query().Get("user"),
			DeviceID: r.URL.Query().Get("device_id"), DeviceName: r.URL.Query().Get("device_name"),
		}
		token := r.URL.Query().Get("token")
		if token == "" || identity.Organization == "" || identity.User == "" || identity.DeviceID == "" {
			http.Error(w, "Incomplete Rein login response.", http.StatusBadRequest)
			once.Do(func() { done <- result{err: errors.New("control plane returned an incomplete enrollment")} })
			return
		}
		fmt.Fprintln(w, "Rein is connected. You may close this window.")
		once.Do(func() { done <- result{identity: identity, token: token} })
	})}
	go server.Serve(listener)
	defer server.Shutdown(context.Background())

	fmt.Printf("Opening Rein Control to enroll %q…\n", *deviceName)
	if err := openBrowser(loginURL.String()); err != nil {
		return fmt.Errorf("open browser: %w\nOpen this URL manually: %s", err, loginURL)
	}
	select {
	case outcome := <-done:
		if outcome.err != nil {
			return outcome.err
		}
		endpoint := cloudURL(*controlURL)
		if err := storeCloudCredential(endpoint, outcome.token); err != nil {
			return err
		}
		profile := cloudProfile{ControlURL: endpoint, Organization: outcome.identity.Organization, User: outcome.identity.User, DeviceID: outcome.identity.DeviceID, DeviceName: outcome.identity.DeviceName, LastContact: time.Now().UTC()}
		if err := saveCloudProfile(profile); err != nil {
			return err
		}
		if *profileName != "" {
			_ = os.WriteFile(filepath.Join(spec.Home(), "active-profile"), []byte(strings.TrimSpace(*profileName)+"\n"), 0o600)
		}
		fmt.Printf("Connected to %s as %s on %s.\n", profile.Organization, profile.User, profile.DeviceName)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func cmdStatus(ctx context.Context, args []string) error {
	fs := flagSet("status")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: rein status")
	}
	profile, err := loadCloudProfile()
	if err != nil {
		return err
	}
	if profile == nil {
		fmt.Println("Rein Control: not connected\nRun `rein login` to enroll this installation.")
		return nil
	}
	token, err := loadCloudCredential(profile.ControlURL)
	if err != nil {
		return err
	}
	identity, err := fetchCloudStatus(ctx, profile.ControlURL, token)
	if err != nil {
		return fmt.Errorf("Rein Control: %w", err)
	}
	profile.Organization, profile.TeamSlug, profile.User, profile.DeviceID, profile.DeviceName = identity.Organization, identity.TeamSlug, identity.User, identity.DeviceID, identity.DeviceName
	profile.LastContact = time.Now().UTC()
	if err := saveCloudProfile(*profile); err != nil {
		return err
	}
	fmt.Print(formatCloudStatus(*profile))
	return nil
}

func formatCloudStatus(profile cloudProfile) string {
	status := fmt.Sprintf("Rein Control\n\nControl URL: %s\nOrganization: %s\n", profile.ControlURL, profile.Organization)
	if team := strings.TrimSpace(profile.TeamSlug); team != "" {
		status += fmt.Sprintf("Team: %s\n", team)
	}
	return status + fmt.Sprintf("User: %s\nDevice: %s\nLast sync: just now\n", profile.User, profile.DeviceName)
}

func cmdSync(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: rein sync")
	}
	p, err := loadCloudProfile()
	if err != nil {
		return err
	}
	if p == nil {
		return errors.New("not connected; run `rein login` first")
	}
	token, err := loadCloudCredential(p.ControlURL)
	if err != nil {
		return err
	}
	version, err := syncPolicy(ctx, p, token)
	if err != nil {
		return err
	}
	fmt.Printf("Policy version %d synced.\n", version)
	return nil
}

// syncPolicy refreshes the local policy cache from the control plane. It is
// shared by the explicit sync command and automatic MCP refreshes.
func syncPolicy(ctx context.Context, p *cloudProfile, token string) (int, error) {
	var bundle struct {
		Version   int             `json:"version"`
		Rules     json.RawMessage `json:"rules"`
		Signature string          `json:"signature"`
		Algorithm string          `json:"signature_algorithm"`
		PublicKey string          `json:"public_key"`
		IssuedAt  string          `json:"issued_at"`
		ExpiresAt string          `json:"expires_at"`
	}
	if err := cloudJSON(ctx, http.MethodGet, p.ControlURL+"/api/v1/rein/policy-bundles/latest", token, nil, &bundle); err != nil {
		return 0, err
	}
	b, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(cloudPolicyPath(), append(b, '\n'), 0o600); err != nil {
		return 0, err
	}
	return bundle.Version, nil
}

// refreshCloudPolicy updates the cache when a connected installation has a
// stale bundle. Failures are intentionally ignored so cached policies remain
// usable offline; explicit `rein sync` still reports errors to the user.
func refreshCloudPolicy(ctx context.Context, maxAge time.Duration) {
	p, err := loadCloudProfile()
	if err != nil || p == nil {
		return
	}
	if info, statErr := os.Stat(cloudPolicyPath()); statErr == nil && time.Since(info.ModTime()) < maxAge {
		return
	}
	token, err := loadCloudCredential(p.ControlURL)
	if err != nil {
		return
	}
	_, _ = syncPolicy(ctx, p, token)
}

func requestCloudApproval(ctx context.Context, caller, tool, intent, command string) {
	p, err := loadCloudProfile()
	if err != nil || p == nil {
		return
	}
	token, err := loadCloudCredential(p.ControlURL)
	if err != nil {
		return
	}
	operation := map[string]any{"tool": tool, "intent": intent}
	if command != "" {
		operation["command"] = command
	}
	if metadata := localPolicyMetadata(caller, tool, intent, command); metadata != nil {
		operation["policy"] = metadata
	}
	canonical, err := json.Marshal(map[string]string{"tool": tool, "intent": intent, "command": command})
	if err != nil {
		return
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(canonical))
	var response map[string]any
	_ = cloudJSON(ctx, http.MethodPost, p.ControlURL+"/api/v1/rein/approvals", token, map[string]any{
		"caller": caller, "operation": operation, "operation_hash": hash,
	}, &response)
}

func cloudApprovalGranted(ctx context.Context, caller, tool, intent string, argv []string) bool {
	p, err := loadCloudProfile()
	if err != nil || p == nil {
		return false
	}
	token, err := loadCloudCredential(p.ControlURL)
	if err != nil {
		return false
	}
	command := strings.Join(argv, " ")
	b, _ := json.Marshal(map[string]string{"tool": tool, "intent": intent, "command": command})
	hash := fmt.Sprintf("%x", sha256.Sum256(b))
	var result struct {
		Approved bool `json:"approved"`
	}
	_ = cloudJSON(ctx, http.MethodGet, p.ControlURL+"/api/v1/rein/approvals/check?operation_hash="+url.QueryEscape(hash)+"&caller="+url.QueryEscape(caller), token, nil, &result)
	return result.Approved
}

func localPolicyMetadata(caller, tool, intent, command string) map[string]any {
	b, err := os.ReadFile(cloudPolicyPath())
	if err != nil {
		return nil
	}
	var bundle struct {
		Version int              `json:"version"`
		Rules   []map[string]any `json:"rules"`
	}
	if json.Unmarshal(b, &bundle) != nil {
		return nil
	}
	for _, rule := range bundle.Rules {
		if !policyWildcardMatch(fmt.Sprint(rule["caller"]), caller) || !policyWildcardMatch(fmt.Sprint(rule["tool"]), tool) {
			continue
		}
		if expected, ok := rule["command"].(string); ok && strings.TrimSpace(expected) != "" && !policyWildcardMatch(expected, command) {
			continue
		}
		if environment, ok := rule["environment"].(string); ok && strings.TrimSpace(environment) != "" && !strings.Contains(strings.ToLower(intent+" "+command), strings.ToLower(environment)) {
			continue
		}
		switch effect := fmt.Sprint(rule["effect"]); effect {
		case "require_approval", "approval_required":
			return map[string]any{"version": bundle.Version, "effect": "require_approval", "reason": "Organization policy requires approval before this operation.", "rule": rule}
		case "allow", "deny":
			return nil
		}
	}
	return nil
}

func policyWildcardMatch(pattern, value string) bool {
	if pattern == "" {
		return true
	}
	matched, err := path.Match(pattern, value)
	return matched || (err != nil && pattern == value)
}

func cmdLogout(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: rein logout")
	}
	p, err := loadCloudProfile()
	if err != nil {
		return err
	}
	if p == nil {
		return errors.New("not connected")
	}
	token, err := loadCloudCredential(p.ControlURL)
	if err != nil {
		return err
	}
	if err := cloudDelete(ctx, p.ControlURL+"/api/v1/rein/device", token); err != nil {
		return err
	}
	if err := deleteCloudCredential(p.ControlURL); err != nil {
		return err
	}
	if err := os.Remove(cloudProfilePath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(cloudAgentsPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("Rein Control device revoked and local credentials removed.")
	return nil
}

func cmdTeam(args []string) error {
	if len(args) == 0 || args[0] == "list" {
		entries, _ := os.ReadDir(spec.Home())
		active := "default"
		if b, err := os.ReadFile(filepath.Join(spec.Home(), "active-profile")); err == nil && strings.TrimSpace(string(b)) != "" {
			active = strings.TrimSpace(string(b))
		}
		fmt.Printf("Active team profile: %s\n", active)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "cloud-") && strings.HasSuffix(e.Name(), ".json") {
				fmt.Println(strings.TrimSuffix(strings.TrimPrefix(e.Name(), "cloud-"), ".json"))
			}
		}
		return nil
	}
	if args[0] != "use" || len(args) != 2 || strings.ContainsAny(args[1], "/\\") {
		return errors.New("usage: rein team list | rein team use <profile>")
	}
	if _, err := os.Stat(filepath.Join(spec.Home(), "cloud-"+args[1]+".json")); err != nil {
		return fmt.Errorf("profile %q not found; run `rein login --profile %s` first", args[1], args[1])
	}
	return os.WriteFile(filepath.Join(spec.Home(), "active-profile"), []byte(args[1]+"\n"), 0o600)
}

func cmdAgent(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: rein agent <register|list|revoke> [name]")
	}
	p, err := loadCloudProfile()
	if err != nil {
		return err
	}
	if p == nil {
		return errors.New("not connected; run `rein login` first")
	}
	token, err := loadCloudCredential(p.ControlURL)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errors.New("usage: rein agent list")
		}
		agents, err := loadAgents()
		if err != nil {
			return err
		}
		if len(agents.Agents) == 0 {
			fmt.Println("No agents registered.")
			return nil
		}
		for _, a := range agents.Agents {
			fmt.Printf("%s\t%s\n", a.Provider, a.ID)
		}
		return nil
	case "register":
		if len(args) != 2 {
			return errors.New("usage: rein agent register <codex|claude-code>")
		}
		provider := strings.ToLower(args[1])
		var reply struct {
			cloudAgent
			Token string `json:"token"`
		}
		if err := cloudJSON(ctx, http.MethodPost, p.ControlURL+"/api/v1/rein/agents", token, map[string]string{"provider": provider}, &reply); err != nil {
			return err
		}
		if reply.ID == "" || reply.Token == "" {
			return errors.New("control plane returned an incomplete agent registration")
		}
		if err := storeAgentCredential(p.ControlURL, reply.ID, reply.Token); err != nil {
			return err
		}
		agents, _ := loadAgents()
		agents.Agents = append(agents.Agents, reply.cloudAgent)
		if err := saveAgents(agents); err != nil {
			return err
		}
		fmt.Printf("Registered %s (%s). Connect it with `rein gateway connect --agent %s`.\n", reply.Provider, reply.ID, reply.Provider)
		return nil
	case "revoke":
		if len(args) != 2 {
			return errors.New("usage: rein agent revoke <name>")
		}
		a, err := registeredAgent(args[1])
		if err != nil {
			return err
		}
		if err := cloudDelete(ctx, p.ControlURL+"/api/v1/rein/agents/"+url.PathEscape(a.ID), token); err != nil {
			return err
		}
		agents, _ := loadAgents()
		kept := cloudAgents{}
		for _, item := range agents.Agents {
			if item.ID != a.ID {
				kept.Agents = append(kept.Agents, item)
			}
		}
		if err := saveAgents(kept); err != nil {
			return err
		}
		_ = deleteAgentCredential(p.ControlURL, a.ID)
		fmt.Printf("Revoked %s.\n", a.Provider)
		return nil
	default:
		return errors.New("usage: rein agent <register|list|revoke> [name]")
	}
}

func registeredAgent(name string) (cloudAgent, error) {
	agents, err := loadAgents()
	if err != nil {
		return cloudAgent{}, err
	}
	for _, a := range agents.Agents {
		if a.Provider == strings.ToLower(name) {
			return a, nil
		}
	}
	return cloudAgent{}, fmt.Errorf("agent %q is not registered; run `rein agent register %s`", name, name)
}

func flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func randomState() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "Rein device"
	}
	return h
}

func openCloudBrowser(rawURL string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", rawURL)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		command = exec.Command("xdg-open", rawURL)
	}
	return command.Start()
}

func credentialAccount(endpoint string) string { return "cloud:" + endpoint }

func storeCloudCredential(endpoint, token string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("secure credential storage is not yet supported on this platform")
	}
	return exec.Command("security", "add-generic-password", "-U", "-s", "rein", "-a", credentialAccount(endpoint), "-w", token).Run()
}

func loadCloudCredential(endpoint string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", errors.New("secure credential storage is not yet supported on this platform")
	}
	b, err := exec.Command("security", "find-generic-password", "-s", "rein", "-a", credentialAccount(endpoint), "-w").Output()
	if err != nil {
		return "", errors.New("credential not found; run `rein login` again")
	}
	return strings.TrimSpace(string(b)), nil
}
func deleteCloudCredential(endpoint string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("secure credential storage is not yet supported on this platform")
	}
	return exec.Command("security", "delete-generic-password", "-s", "rein", "-a", credentialAccount(endpoint)).Run()
}
func storeAgentCredential(endpoint, id, token string) error {
	return storeCloudCredential(endpoint+":"+id, token)
}
func loadAgentCredential(endpoint, id string) (string, error) {
	return loadCloudCredential(endpoint + ":" + id)
}
func deleteAgentCredential(endpoint, id string) error {
	return deleteCloudCredential(endpoint + ":" + id)
}

func cloudJSON(ctx context.Context, method, endpoint, token string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := cloudHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("request returned %s", resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
func cloudDelete(ctx context.Context, endpoint, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := cloudHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("request returned %s", resp.Status)
	}
	return nil
}

func emitAudit(ctx context.Context, caller, tool, intent, event string) error {
	p, err := loadCloudProfile()
	if err != nil || p == nil {
		return err
	}
	token, err := loadCloudCredential(p.ControlURL)
	if err != nil {
		return err
	}
	return cloudJSON(ctx, http.MethodPost, p.ControlURL+"/api/v1/rein/audit-events", token, map[string]any{"event": event, "caller": caller, "operation": map[string]string{"tool": tool, "intent": intent}}, nil)
}

func fetchCloudStatus(ctx context.Context, endpoint, token string) (cloudIdentity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/v1/rein/status", nil)
	if err != nil {
		return cloudIdentity{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := cloudHTTPClient.Do(req)
	if err != nil {
		return cloudIdentity{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return cloudIdentity{}, fmt.Errorf("status request returned %s", resp.Status)
	}
	var identity cloudIdentity
	if err := json.NewDecoder(resp.Body).Decode(&identity); err != nil {
		return cloudIdentity{}, err
	}
	if identity.Organization == "" || identity.User == "" || identity.DeviceID == "" {
		return cloudIdentity{}, errors.New("status response is incomplete")
	}
	return identity, nil
}
