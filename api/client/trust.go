package client

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/docker/distribution/digest"
	"github.com/docker/distribution/registry/client/auth"
	"github.com/docker/distribution/registry/client/transport"
	"github.com/docker/docker/cliconfig"
	"github.com/docker/docker/pkg/ansiescape"
	"github.com/docker/docker/pkg/ioutils"
	flag "github.com/docker/docker/pkg/mflag"
	"github.com/docker/docker/reference"
	"github.com/docker/docker/registry"
	apiclient "github.com/docker/engine-api/client"
	"github.com/docker/engine-api/types"
	registrytypes "github.com/docker/engine-api/types/registry"
	"github.com/docker/go-connections/tlsconfig"
	"github.com/docker/notary/client"
	"github.com/docker/notary/passphrase"
	"github.com/docker/notary/trustmanager"
	"github.com/docker/notary/tuf/data"
)

var untrusted bool

func addTrustedFlags(fs *flag.FlagSet, verify bool) {
	var trusted bool
	if e := os.Getenv("DOCKER_CONTENT_TRUST"); e != "" {
		if t, err := strconv.ParseBool(e); t || err != nil {
			// treat any other value as true
			trusted = true
		}
	}
	message := "Skip image signing"
	if verify {
		message = "Skip image verification"
	}
	fs.BoolVar(&untrusted, []string{"-disable-content-trust"}, !trusted, message)
}

func isTrusted() bool {
	return !untrusted
}

var targetRegexp = regexp.MustCompile(`([\S]+): digest: ([\S]+) size: ([\d]+)`)

type target struct {
	reference registry.Reference
	digest    digest.Digest
	size      int64
}

func (cli *DockerCli) trustDirectory() string {
	return filepath.Join(cliconfig.ConfigDir(), "trust")
}

// certificateDirectory returns the directory containing
// TLS certificates for the given server. An error is
// returned if there was an error parsing the server string.
func (cli *DockerCli) certificateDirectory(server string) (string, error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", err
	}

	return filepath.Join(cliconfig.ConfigDir(), "tls", u.Host), nil
}

func trustServer(index *registrytypes.IndexInfo) (string, error) {
	if s := os.Getenv("DOCKER_CONTENT_TRUST_SERVER"); s != "" {
		urlObj, err := url.Parse(s)
		if err != nil || urlObj.Scheme != "https" {
			return "", fmt.Errorf("valid https URL required for trust server, got %s", s)
		}

		return s, nil
	}
	if index.Official {
		return registry.NotaryServer, nil
	}
	return "https://" + index.Name, nil
}

type simpleCredentialStore struct {
	auth types.AuthConfig
}

func (scs simpleCredentialStore) Basic(u *url.URL) (string, string) {
	return scs.auth.Username, scs.auth.Password
}

func (cli *DockerCli) getNotaryRepository(repoInfo *registry.RepositoryInfo, authConfig types.AuthConfig) (*client.NotaryRepository, error) {
	server, err := trustServer(repoInfo.Index)
	if err != nil {
		return nil, err
	}

	var cfg = tlsconfig.ClientDefault
	cfg.InsecureSkipVerify = !repoInfo.Index.Secure

	// Get certificate base directory
	certDir, err := cli.certificateDirectory(server)
	if err != nil {
		return nil, err
	}
	logrus.Debugf("reading certificate directory: %s", certDir)

	if err := registry.ReadCertsDirectory(&cfg, certDir); err != nil {
		return nil, err
	}

	base := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		Dial: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).Dial,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     &cfg,
		DisableKeepAlives:   true,
	}

	// Skip configuration headers since request is not going to Docker daemon
	modifiers := registry.DockerHeaders(http.Header{})
	authTransport := transport.NewTransport(base, modifiers...)
	pingClient := &http.Client{
		Transport: authTransport,
		Timeout:   5 * time.Second,
	}
	endpointStr := server + "/v2/"
	req, err := http.NewRequest("GET", endpointStr, nil)
	if err != nil {
		return nil, err
	}

	challengeManager := auth.NewSimpleChallengeManager()

	resp, err := pingClient.Do(req)
	if err != nil {
		// Ignore error on ping to operate in offline mode
		logrus.Debugf("Error pinging notary server %q: %s", endpointStr, err)
	} else {
		defer resp.Body.Close()

		// Add response to the challenge manager to parse out
		// authentication header and register authentication method
		if err := challengeManager.AddResponse(resp); err != nil {
			return nil, err
		}
	}

	creds := simpleCredentialStore{auth: authConfig}
	tokenHandler := auth.NewTokenHandler(authTransport, creds, repoInfo.FullName(), "push", "pull")
	basicHandler := auth.NewBasicHandler(creds)
	modifiers = append(modifiers, transport.RequestModifier(auth.NewAuthorizer(challengeManager, tokenHandler, basicHandler)))
	tr := transport.NewTransport(base, modifiers...)

	return client.NewNotaryRepository(cli.trustDirectory(), repoInfo.FullName(), server, tr, cli.getPassphraseRetriever())
}

func convertTarget(t client.Target) (target, error) {
	h, ok := t.Hashes["sha256"]
	if !ok {
		return target{}, errors.New("no valid hash, expecting sha256")
	}
	return target{
		reference: registry.ParseReference(t.Name),
		digest:    digest.NewDigestFromHex("sha256", hex.EncodeToString(h)),
		size:      t.Length,
	}, nil
}

func (cli *DockerCli) getPassphraseRetriever() passphrase.Retriever {
	aliasMap := map[string]string{
		"root":     "root",
		"snapshot": "repository",
		"targets":  "repository",
	}
	baseRetriever := passphrase.PromptRetrieverWithInOut(cli.in, cli.out, aliasMap)
	env := map[string]string{
		"root":     os.Getenv("DOCKER_CONTENT_TRUST_ROOT_PASSPHRASE"),
		"snapshot": os.Getenv("DOCKER_CONTENT_TRUST_REPOSITORY_PASSPHRASE"),
		"targets":  os.Getenv("DOCKER_CONTENT_TRUST_REPOSITORY_PASSPHRASE"),
	}

	// Backwards compatibility with old env names. We should remove this in 1.10
	if env["root"] == "" {
		if passphrase := os.Getenv("DOCKER_CONTENT_TRUST_OFFLINE_PASSPHRASE"); passphrase != "" {
			env["root"] = passphrase
			fmt.Fprintf(cli.err, "[DEPRECATED] The environment variable DOCKER_CONTENT_TRUST_OFFLINE_PASSPHRASE has been deprecated and will be removed in v1.10. Please use DOCKER_CONTENT_TRUST_ROOT_PASSPHRASE\n")
		}
	}
	if env["snapshot"] == "" || env["targets"] == "" {
		if passphrase := os.Getenv("DOCKER_CONTENT_TRUST_TAGGING_PASSPHRASE"); passphrase != "" {
			env["snapshot"] = passphrase
			env["targets"] = passphrase
			fmt.Fprintf(cli.err, "[DEPRECATED] The environment variable DOCKER_CONTENT_TRUST_TAGGING_PASSPHRASE has been deprecated and will be removed in v1.10. Please use DOCKER_CONTENT_TRUST_REPOSITORY_PASSPHRASE\n")
		}
	}

	return func(keyName string, alias string, createNew bool, numAttempts int) (string, bool, error) {
		if v := env[alias]; v != "" {
			return v, numAttempts > 1, nil
		}
		return baseRetriever(keyName, alias, createNew, numAttempts)
	}
}

func (cli *DockerCli) trustedReference(ref reference.NamedTagged) (reference.Canonical, error) {
	repoInfo, err := registry.ParseRepositoryInfo(ref)
	if err != nil {
		return nil, err
	}

	// Resolve the Auth config relevant for this server
	authConfig := registry.ResolveAuthConfig(cli.configFile.AuthConfigs, repoInfo.Index)

	notaryRepo, err := cli.getNotaryRepository(repoInfo, authConfig)
	if err != nil {
		fmt.Fprintf(cli.out, "Error establishing connection to trust repository: %s\n", err)
		return nil, err
	}

	t, err := notaryRepo.GetTargetByName(ref.Tag())
	if err != nil {
		return nil, err
	}
	r, err := convertTarget(*t)
	if err != nil {
		return nil, err

	}

	return reference.WithDigest(ref, r.digest)
}

func (cli *DockerCli) tagTrusted(trustedRef reference.Canonical, ref reference.NamedTagged) error {
	fmt.Fprintf(cli.out, "Tagging %s as %s\n", trustedRef.String(), ref.String())

	options := types.ImageTagOptions{
		ImageID:        trustedRef.String(),
		RepositoryName: trustedRef.Name(),
		Tag:            ref.Tag(),
		Force:          true,
	}

	return cli.client.ImageTag(options)
}

func notaryError(err error) error {
	switch err.(type) {
	case *json.SyntaxError:
		logrus.Debugf("Notary syntax error: %s", err)
		return errors.New("no trust data available for remote repository")
	case client.ErrExpired:
		return fmt.Errorf("remote repository out-of-date: %v", err)
	case trustmanager.ErrKeyNotFound:
		return fmt.Errorf("signing keys not found: %v", err)
	case *net.OpError:
		return fmt.Errorf("error contacting notary server: %v", err)
	}

	return err
}

func (cli *DockerCli) trustedPull(repoInfo *registry.RepositoryInfo, ref registry.Reference, authConfig types.AuthConfig, requestPrivilege apiclient.RequestPrivilegeFunc) error {
	var refs []target

	notaryRepo, err := cli.getNotaryRepository(repoInfo, authConfig)
	if err != nil {
		fmt.Fprintf(cli.out, "Error establishing connection to trust repository: %s\n", err)
		return err
	}

	if ref.String() == "" {
		// List all targets
		targets, err := notaryRepo.ListTargets()
		if err != nil {
			return notaryError(err)
		}
		for _, tgt := range targets {
			t, err := convertTarget(*tgt)
			if err != nil {
				fmt.Fprintf(cli.out, "Skipping target for %q\n", repoInfo.Name())
				continue
			}
			refs = append(refs, t)
		}
	} else {
		t, err := notaryRepo.GetTargetByName(ref.String())
		if err != nil {
			return notaryError(err)
		}
		r, err := convertTarget(*t)
		if err != nil {
			return err

		}
		refs = append(refs, r)
	}

	for i, r := range refs {
		displayTag := r.reference.String()
		if displayTag != "" {
			displayTag = ":" + displayTag
		}
		fmt.Fprintf(cli.out, "Pull (%d of %d): %s%s@%s\n", i+1, len(refs), repoInfo.Name(), displayTag, r.digest)

		if err := cli.imagePullPrivileged(authConfig, repoInfo.Name(), r.digest.String(), requestPrivilege); err != nil {
			return err
		}

		// If reference is not trusted, tag by trusted reference
		if !r.reference.HasDigest() {
			tagged, err := reference.WithTag(repoInfo, r.reference.String())
			if err != nil {
				return err
			}
			trustedRef, err := reference.WithDigest(repoInfo, r.digest)
			if err != nil {
				return err
			}
			if err := cli.tagTrusted(trustedRef, tagged); err != nil {
				return err
			}
		}
	}
	return nil
}

func targetStream(in io.Writer) (io.WriteCloser, <-chan []target) {
	r, w := io.Pipe()
	out := io.MultiWriter(in, w)
	targetChan := make(chan []target)

	go func() {
		targets := []target{}
		scanner := bufio.NewScanner(r)
		scanner.Split(ansiescape.ScanANSILines)
		for scanner.Scan() {
			line := scanner.Bytes()
			if matches := targetRegexp.FindSubmatch(line); len(matches) == 4 {
				dgst, err := digest.ParseDigest(string(matches[2]))
				if err != nil {
					// Line does match what is expected, continue looking for valid lines
					logrus.Debugf("Bad digest value %q in matched line, ignoring\n", string(matches[2]))
					continue
				}
				s, err := strconv.ParseInt(string(matches[3]), 10, 64)
				if err != nil {
					// Line does match what is expected, continue looking for valid lines
					logrus.Debugf("Bad size value %q in matched line, ignoring\n", string(matches[3]))
					continue
				}

				targets = append(targets, target{
					reference: registry.ParseReference(string(matches[1])),
					digest:    dgst,
					size:      s,
				})
			}
		}
		targetChan <- targets
	}()

	return ioutils.NewWriteCloserWrapper(out, w.Close), targetChan
}

func (cli *DockerCli) trustedPush(repoInfo *registry.RepositoryInfo, tag string, authConfig types.AuthConfig, requestPrivilege apiclient.RequestPrivilegeFunc) error {
	streamOut, targetChan := targetStream(cli.out)

	reqError := cli.imagePushPrivileged(authConfig, repoInfo.Name(), tag, streamOut, requestPrivilege)

	// Close stream channel to finish target parsing
	if err := streamOut.Close(); err != nil {
		return err
	}
	// Check error from request
	if reqError != nil {
		return reqError
	}

	// Get target results
	targets := <-targetChan

	if tag == "" {
		fmt.Fprintf(cli.out, "No tag specified, skipping trust metadata push\n")
		return nil
	}
	if len(targets) == 0 {
		fmt.Fprintf(cli.out, "No targets found, skipping trust metadata push\n")
		return nil
	}

	fmt.Fprintf(cli.out, "Signing and pushing trust metadata\n")

	repo, err := cli.getNotaryRepository(repoInfo, authConfig)
	if err != nil {
		fmt.Fprintf(cli.out, "Error establishing connection to notary repository: %s\n", err)
		return err
	}

	for _, target := range targets {
		h, err := hex.DecodeString(target.digest.Hex())
		if err != nil {
			return err
		}
		t := &client.Target{
			Name: target.reference.String(),
			Hashes: data.Hashes{
				string(target.digest.Algorithm()): h,
			},
			Length: int64(target.size),
		}
		if err := repo.AddTarget(t); err != nil {
			return err
		}
	}

	err = repo.Publish()
	if _, ok := err.(*client.ErrRepoNotInitialized); !ok {
		return notaryError(err)
	}

	keys := repo.CryptoService.ListKeys(data.CanonicalRootRole)

	var rootKeyID string
	// always select the first root key
	if len(keys) > 0 {
		sort.Strings(keys)
		rootKeyID = keys[0]
	} else {
		rootPublicKey, err := repo.CryptoService.Create(data.CanonicalRootRole, data.ECDSAKey)
		if err != nil {
			return err
		}
		rootKeyID = rootPublicKey.ID()
	}

	if err := repo.Initialize(rootKeyID); err != nil {
		return notaryError(err)
	}
	fmt.Fprintf(cli.out, "Finished initializing %q\n", repoInfo.FullName())

	return notaryError(repo.Publish())
}
