package klient

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"
)

type ExecConfig struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
	Env     []struct {
		Name  string `yaml:"name"`
		Value string `yaml:"value"`
	} `yaml:"env"`
}

type KubeConfig struct {
	Clusters []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server                   string `yaml:"server"`
			CertificateAuthorityData string `yaml:"certificate-authority-data"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			Token                 string      `yaml:"token"`
			ClientCertificateData string      `yaml:"client-certificate-data"`
			ClientKeyData         string      `yaml:"client-key-data"`
			Exec                  *ExecConfig `yaml:"exec"`
		} `yaml:"user"`
	} `yaml:"users"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
	CurrentContext string `yaml:"current-context"`
}

func LoadConfig() (*KubeConfig, error) {
	path := os.Getenv("KUBECONFIG")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, ".kube", "config")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var config KubeConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

type clientContext struct {
	ServerURL string
	Token     string
	TLSConfig *tls.Config
}

func getClientContext() (*clientContext, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host != "" && port != "" {
		// in-cluster config
		tokenData, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
		if err == nil {
			caData, _ := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
			tlsConfig := &tls.Config{}
			if len(caData) > 0 {
				caCertPool := x509.NewCertPool()
				caCertPool.AppendCertsFromPEM(caData)
				tlsConfig.RootCAs = caCertPool
			} else {
				tlsConfig.InsecureSkipVerify = true
			}
			return &clientContext{
				ServerURL: fmt.Sprintf("https://%s:%s", host, port),
				Token:     string(tokenData),
				TLSConfig: tlsConfig,
			}, nil
		}
	}

	cfg, err := LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	var contextName = cfg.CurrentContext
	if contextName == "" {
		return nil, errors.New("no current-context in kubeconfig")
	}

	var clusterName, userName string
	for _, ctx := range cfg.Contexts {
		if ctx.Name == contextName {
			clusterName = ctx.Context.Cluster
			userName = ctx.Context.User
			break
		}
	}

	var serverURL, caData string
	for _, c := range cfg.Clusters {
		if c.Name == clusterName {
			serverURL = c.Cluster.Server
			caData = c.Cluster.CertificateAuthorityData
			break
		}
	}

	var token, certData, keyData string
	var execCfg *ExecConfig
	for _, u := range cfg.Users {
		if u.Name == userName {
			token = u.User.Token
			certData = u.User.ClientCertificateData
			keyData = u.User.ClientKeyData
			execCfg = u.User.Exec
			break
		}
	}

	if serverURL == "" {
		return nil, errors.New("cluster not found or missing server URL")
	}

	tlsConfig := &tls.Config{}
	
	if caData != "" {
		caCert, err := base64.StdEncoding.DecodeString(caData)
		if err == nil {
			caCertPool := x509.NewCertPool()
			caCertPool.AppendCertsFromPEM(caCert)
			tlsConfig.RootCAs = caCertPool
		}
	} else {
		tlsConfig.InsecureSkipVerify = true
	}

	if certData != "" && keyData != "" {
		certBytes, _ := base64.StdEncoding.DecodeString(certData)
		keyBytes, _ := base64.StdEncoding.DecodeString(keyData)
		cert, err := tls.X509KeyPair(certBytes, keyBytes)
		if err == nil {
			tlsConfig.Certificates = []tls.Certificate{cert}
		}
	}

	if execCfg != nil {
		t, err := executeExecPlugin(execCfg)
		if err != nil {
			return nil, fmt.Errorf("exec plugin failed: %w", err)
		}
		token = t
	}

	return &clientContext{
		ServerURL: serverURL,
		Token:     token,
		TLSConfig: tlsConfig,
	}, nil
}

func executeExecPlugin(cfg *ExecConfig) (string, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	for _, env := range cfg.Env {
		cmd.Env = append(cmd.Env, env.Name+"="+env.Value)
	}
	cmd.Env = append(cmd.Env, os.Environ()...)
	
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}

	var result struct {
		Status struct {
			Token string `json:"token"`
		} `json:"status"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return "", err
	}
	return result.Status.Token, nil
}

// Dial connects to a pod and returns a net.Conn
func Dial(namespace, pod string, port int) (net.Conn, error) {
	ctx, err := getClientContext()
	if err != nil {
		return nil, err
	}

	header := http.Header{}
	if ctx.Token != "" {
		header.Add("Authorization", "Bearer "+ctx.Token)
	}

	u, err := url.Parse(ctx.ServerURL)
	if err != nil {
		return nil, err
	}
	
	wsScheme := "wss"
	if u.Scheme == "http" {
		wsScheme = "ws"
	}

	wsURL := fmt.Sprintf("%s://%s/api/v1/namespaces/%s/pods/%s/portforward?ports=%d", wsScheme, u.Host, namespace, pod, port)

	dialer := websocket.Dialer{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: ctx.TLSConfig,
		Subprotocols:    []string{"v4.channel.k8s.io"},
	}

	ws, resp, err := dialer.Dial(wsURL, header)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("dial failed: %w (status: %s)", err, resp.Status)
		}
		return nil, fmt.Errorf("dial failed: %w", err)
	}

	return &wsConn{ws: ws}, nil
}

type PodList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	} `json:"items"`
}

func ListPods(namespace, labelSelector string) ([]string, error) {
	ctx, err := getClientContext()
	if err != nil {
		return nil, err
	}

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: ctx.TLSConfig,
		},
	}

	u, err := url.Parse(ctx.ServerURL)
	if err != nil {
		return nil, err
	}
	
	u.Path = fmt.Sprintf("/api/v1/namespaces/%s/pods", namespace)
	q := u.Query()
	q.Set("labelSelector", labelSelector)
	u.RawQuery = q.Encode()

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, err
	}

	if ctx.Token != "" {
		req.Header.Set("Authorization", "Bearer "+ctx.Token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %s", resp.Status)
	}

	var podList PodList
	if err := json.NewDecoder(resp.Body).Decode(&podList); err != nil {
		return nil, err
	}

	var names []string
	for _, item := range podList.Items {
		names = append(names, item.Metadata.Name)
	}

	return names, nil
}
