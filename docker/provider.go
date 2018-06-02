package docker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/hashicorp/terraform/helper/schema"
	"github.com/hashicorp/terraform/terraform"
)

// Provider creates the Docker provider
func Provider() terraform.ResourceProvider {
	return &schema.Provider{
		Schema: map[string]*schema.Schema{
			"host": &schema.Schema{
				Type:        schema.TypeString,
				Required:    true,
				DefaultFunc: schema.EnvDefaultFunc("DOCKER_HOST", "unix:///var/run/docker.sock"),
				Description: "The Docker daemon address",
			},

			"ca_material": &schema.Schema{
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("DOCKER_CA_MATERIAL", ""),
				Description: "PEM-encoded content of Docker host CA certificate",
			},
			"cert_material": &schema.Schema{
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("DOCKER_CERT_MATERIAL", ""),
				Description: "PEM-encoded content of Docker client certificate",
			},
			"key_material": &schema.Schema{
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("DOCKER_KEY_MATERIAL", ""),
				Description: "PEM-encoded content of Docker client private key",
			},

			"cert_path": &schema.Schema{
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("DOCKER_CERT_PATH", ""),
				Description: "Path to directory with Docker TLS config",
			},

			"registry_auth": &schema.Schema{
				Type:     schema.TypeSet,
				Optional: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"address": &schema.Schema{
							Type:        schema.TypeString,
							Required:    true,
							Description: "Address of the registry",
						},

						"username": &schema.Schema{
							Type:          schema.TypeString,
							Optional:      true,
							ConflictsWith: []string{"registry_auth.config_file"},
							DefaultFunc:   schema.EnvDefaultFunc("DOCKER_REGISTRY_USER", ""),
							Description:   "Username for the registry",
						},

						"password": &schema.Schema{
							Type:          schema.TypeString,
							Optional:      true,
							Sensitive:     true,
							ConflictsWith: []string{"registry_auth.config_file"},
							DefaultFunc:   schema.EnvDefaultFunc("DOCKER_REGISTRY_PASS", ""),
							Description:   "Password for the registry",
						},

						"config_file": &schema.Schema{
							Type:          schema.TypeString,
							Optional:      true,
							ConflictsWith: []string{"registry_auth.username", "registry_auth.password"},
							DefaultFunc:   schema.EnvDefaultFunc("DOCKER_CONFIG", "~/.docker/config.json"),
							Description:   "Path to docker json file for registry auth",
						},
					},
				},
			},
		},

		ResourcesMap: map[string]*schema.Resource{
			"docker_container": resourceDockerContainer(),
			"docker_image":     resourceDockerImage(),
			"docker_network":   resourceDockerNetwork(),
			"docker_volume":    resourceDockerVolume(),
			"docker_config":    resourceDockerConfig(),
			"docker_secret":    resourceDockerSecret(),
			// "docker_service":   resourceDockerService(),
		},

		DataSourcesMap: map[string]*schema.Resource{
			"docker_registry_image": dataSourceDockerRegistryImage(),
		},

		ConfigureFunc: providerConfigure,
	}
}

func providerConfigure(d *schema.ResourceData) (interface{}, error) {
	config := Config{
		Host:     d.Get("host").(string),
		Ca:       d.Get("ca_material").(string),
		Cert:     d.Get("cert_material").(string),
		Key:      d.Get("key_material").(string),
		CertPath: d.Get("cert_path").(string),
	}

	client, err := config.NewClient()
	if err != nil {
		return nil, fmt.Errorf("Error initializing Docker client: %s", err)
	}

	ctx := context.Background()
	_, err = client.Ping(ctx)
	if err != nil {
		return nil, fmt.Errorf("Error pinging Docker server: %s", err)
	}

	authConfigs := &AuthConfigs{}

	if v, ok := d.GetOk("registry_auth"); ok {
		authConfigs, err = providerSetToRegistryAuth(v.(*schema.Set))

		if err != nil {
			return nil, fmt.Errorf("Error loading registry auth config: %s", err)
		}
	}

	providerConfig := ProviderConfig{
		DockerClient: client,
		AuthConfigs:  authConfigs,
	}

	return &providerConfig, nil
}

// ErrCannotParseDockercfg is the error returned by NewAuthConfigurations when the dockercfg cannot be parsed.
var ErrCannotParseDockercfg = errors.New("Failed to read authentication from dockercfg")

// AuthConfigs represents authentication options to use for the
// PushImage method accommodating the new X-Registry-Config header
type AuthConfigs struct {
	Configs map[string]types.AuthConfig `json:"configs"`
}

// dockerConfig represents a registry authentation configuration from the
// .dockercfg file.
type dockerConfig struct {
	Auth  string `json:"auth"`
	Email string `json:"email"`
}

// Take the given registry_auth schemas and return a map of registry auth configurations
func providerSetToRegistryAuth(authSet *schema.Set) (*AuthConfigs, error) {
	authConfigs := AuthConfigs{
		Configs: make(map[string]types.AuthConfig),
	}

	for _, authInt := range authSet.List() {
		auth := authInt.(map[string]interface{})
		authConfig := types.AuthConfig{}
		authConfig.ServerAddress = normalizeRegistryAddress(auth["address"].(string))

		// For each registry_auth block, generate an AuthConfiguration using either
		// username/password or the given config file
		if username, ok := auth["username"]; ok && username.(string) != "" {
			authConfig.Username = auth["username"].(string)
			authConfig.Password = auth["password"].(string)
		} else if configFile, ok := auth["config_file"]; ok && configFile.(string) != "" {
			filePath := configFile.(string)
			if strings.HasPrefix(filePath, "~/") {
				usr, err := user.Current()
				if err != nil {
					return nil, err
				}
				filePath = strings.Replace(filePath, "~", usr.HomeDir, 1)
			}

			r, err := os.Open(filePath)
			if err != nil {
				return nil, fmt.Errorf("Error opening docker registry config file: %v", err)
			}

			auths, err := newAuthConfigurations(r)
			if err != nil {
				return nil, fmt.Errorf("Error parsing docker registry config json: %v", err)
			}

			foundRegistry := false
			for registry, authFileConfig := range auths.Configs {
				if authConfig.ServerAddress == normalizeRegistryAddress(registry) {
					authConfig.Username = authFileConfig.Username
					authConfig.Password = authFileConfig.Password
					foundRegistry = true
				}
			}

			if !foundRegistry {
				return nil, fmt.Errorf("Couldn't find registry config for '%s' in file: %s",
					authConfig.ServerAddress, filePath)
			}
		}

		authConfigs.Configs[authConfig.ServerAddress] = authConfig
	}

	return &authConfigs, nil
}

// newAuthConfigurations returns AuthConfigs from a JSON encoded string in the
// same format as the .dockercfg file.
func newAuthConfigurations(r io.Reader) (*AuthConfigs, error) {
	var auth *AuthConfigs
	confs, err := parseDockerConfig(r)
	if err != nil {
		return nil, err
	}
	auth, err = authConfigs(confs)
	if err != nil {
		return nil, err
	}
	return auth, nil
}

// parseDockerConfig parses the docker config file for auths
func parseDockerConfig(r io.Reader) (map[string]dockerConfig, error) {
	buf := new(bytes.Buffer)
	buf.ReadFrom(r)
	byteData := buf.Bytes()

	confsWrapper := struct {
		Auths map[string]dockerConfig `json:"auths"`
	}{}
	if err := json.Unmarshal(byteData, &confsWrapper); err == nil {
		if len(confsWrapper.Auths) > 0 {
			return confsWrapper.Auths, nil
		}
	}

	var confs map[string]dockerConfig
	if err := json.Unmarshal(byteData, &confs); err != nil {
		return nil, err
	}
	return confs, nil
}

// authConfigs converts a dockerConfigs map to a AuthConfigs object.
func authConfigs(confs map[string]dockerConfig) (*AuthConfigs, error) {
	c := &AuthConfigs{
		Configs: make(map[string]types.AuthConfig),
	}
	for reg, conf := range confs {
		if conf.Auth == "" {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(conf.Auth)
		if err != nil {
			return nil, err
		}
		userpass := strings.SplitN(string(data), ":", 2)
		if len(userpass) != 2 {
			return nil, ErrCannotParseDockercfg
		}
		c.Configs[reg] = types.AuthConfig{
			Email:         conf.Email,
			Username:      userpass[0],
			Password:      userpass[1],
			ServerAddress: reg,
			Auth:          conf.Auth,
		}
	}
	return c, nil
}

// TODO
// AuthCheck validates the given credentials. It returns nil if successful.
//
// For Docker API versions >= 1.23, the AuthStatus struct will be populated, otherwise it will be empty.`
//
// See https://goo.gl/6nsZkH for more details.
// func (c *Client) AuthCheck(conf *AuthConfiguration) (AuthStatus, error) {
// 	var authStatus AuthStatus
// 	if conf == nil {
// 		return authStatus, errors.New("conf is nil")
// 	}
// 	resp, err := c.do("POST", "/auth", doOptions{data: conf})
// 	if err != nil {
// 		return authStatus, err
// 	}
// 	defer resp.Body.Close()
// 	data, err := ioutil.ReadAll(resp.Body)
// 	if err != nil {
// 		return authStatus, err
// 	}
// 	if len(data) == 0 {
// 		return authStatus, nil
// 	}
// 	if err := json.Unmarshal(data, &authStatus); err != nil {
// 		return authStatus, err
// 	}
// 	return authStatus, nil
// }
