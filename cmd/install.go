package cmd

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	operator "github.com/alexellis/k3sup/pkg/operator"

	homedir "github.com/mitchellh/go-homedir"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/terminal"
)

var kubeconfig []byte

type k3sExecOptions struct {
	Datastore    string
	ExtraArgs    string
	FlannelIPSec bool
	NoExtras     bool
}

func MakeInstall() *cobra.Command {
	var command = &cobra.Command{
		Use:          "install",
		Short:        "Install k3s on a server via SSH",
		Long:         `Install k3s on a server via SSH.`,
		Example:      `  k3sup install --ip 192.168.0.100 --user root`,
		SilenceUsage: true,
	}

	command.Flags().IP("ip", net.ParseIP("127.0.0.1"), "Public IP of node")
	command.Flags().String("user", "root", "Username for SSH login")

	command.Flags().String("ssh-key", "", "The ssh key to use for remote login")
	command.Flags().Int("ssh-port", 22, "The port on which to connect for ssh")
	command.Flags().Bool("sudo", true, "Use sudo for installation. e.g. set to false when using the root user and no sudo is available.")
	command.Flags().Bool("skip-install", false, "Skip the k3s installer")
	command.Flags().String("local-path", "kubeconfig", "Local path to save the kubeconfig file")
	command.Flags().String("context", "default", "Set the name of the kubeconfig context.")
	command.Flags().Bool("no-extras", false, `Disable "servicelb" and "traefik"`)

	command.Flags().Bool("ipsec", false, "Enforces and/or activates optional extra argument for k3s: flannel-backend option: ipsec")
	command.Flags().Bool("merge", false, `Merge the config with existing kubeconfig if it already exists.
Provide the --local-path flag with --merge if a kubeconfig already exists in some other directory`)
	command.Flags().Bool("local", false, "Perform a local install without using ssh")
	command.Flags().Bool("cluster", false, "Form a dqlite cluster")

	command.Flags().Bool("print-command", false, "Print a command that you can use with SSH to manually recover from an error")
	command.Flags().String("datastore", "", "Optional: connection-string for the k3s datastore to enable HA, i.e. \"mysql://username:password@tcp(hostname:3306)/database-name\"")

	command.Flags().String("k3s-version", "", "Optional: set a version to install, overrides k3s-channel")
	command.Flags().String("k3s-extra-args", "", "Optional extra arguments to pass to k3s installer, wrapped in quotes (e.g. --k3s-extra-args '--no-deploy servicelb')")
	command.Flags().String("k3s-channel", "v1.18", "Optional release channel: stable, latest, or i.e. v1.18")

	command.Flags().String("tls-san", "", "Optional: defaults to server IP, unless provided")

	command.RunE = func(command *cobra.Command, args []string) error {

		fmt.Printf("Running: k3sup install\n")

		localKubeconfig, _ := command.Flags().GetString("local-path")

		skipInstall, err := command.Flags().GetBool("skip-install")
		if err != nil {
			return err
		}
		tlsSAN, _ := command.Flags().GetString("tls-san")

		useSudo, err := command.Flags().GetBool("sudo")
		if err != nil {
			return err
		}

		sudoPrefix := ""
		if useSudo {
			sudoPrefix = "sudo "
		}

		k3sVersion, err := command.Flags().GetString("k3s-version")
		if err != nil {
			return err
		}
		k3sExtraArgs, err := command.Flags().GetString("k3s-extra-args")
		if err != nil {
			return err
		}
		k3sChannel, err := command.Flags().GetString("k3s-channel")
		if err != nil {
			return err
		}
		k3sNoExtras, err := command.Flags().GetBool("no-extras")
		if err != nil {
			return err
		}

		flannelIPSec, _ := command.Flags().GetBool("ipsec")

		local, _ := command.Flags().GetBool("local")

		ip, _ := command.Flags().GetIP("ip")

		cluster, _ := command.Flags().GetBool("cluster")
		datastore, _ := command.Flags().GetString("datastore")
		printCommand, err := command.Flags().GetBool("print-command")
		if err != nil {
			return err
		}

		merge, err := command.Flags().GetBool("merge")
		if err != nil {
			return err
		}
		context, err := command.Flags().GetString("context")
		if err != nil {
			return err
		}

		if len(datastore) > 0 {
			if strings.Index(datastore, "ssl-mode=REQUIRED") > -1 {
				return fmt.Errorf("remove ssl-mode=REQUIRED from your datastore string, it is not supported by the k3s syntax")
			}
			if strings.Index(datastore, "mysql") > -1 && strings.Index(datastore, "tcp") == -1 {
				return fmt.Errorf("you must specify the mysql host as tcp(host:port) or tcp(ip:port), see the k3s docs for more: https://rancher.com/docs/k3s/latest/en/installation/ha")
			}
		}

		installk3sExec := makeInstallExec(cluster, ip, tlsSAN,
			k3sExecOptions{
				Datastore:    datastore,
				FlannelIPSec: flannelIPSec,
				NoExtras:     k3sNoExtras,
				ExtraArgs:    k3sExtraArgs,
			})

		if len(k3sVersion) == 0 && len(k3sChannel) == 0 {
			return fmt.Errorf("give a value for --k3s-version or --k3s-channel")
		}

		installStr := createVersionStr(k3sVersion, k3sChannel)

		installK3scommand := fmt.Sprintf("%s | %s %s sh -\n", getScript, installk3sExec, installStr)

		getConfigcommand := fmt.Sprintf(sudoPrefix + "cat /etc/rancher/k3s/k3s.yaml\n")

		if local {
			operator := operator.ExecOperator{}

			fmt.Printf("Executing: %s\n", installK3scommand)

			res, err := operator.Execute(installK3scommand)
			if err != nil {
				return err
			}

			if len(res.StdErr) > 0 {
				fmt.Printf("stderr: %q", res.StdErr)
			}
			if len(res.StdOut) > 0 {
				fmt.Printf("stdout: %q", res.StdOut)
			}

			err = obtainKubeconfig(operator, getConfigcommand, ip.String(), context, localKubeconfig, merge)
			if err != nil {
				return err
			}

			return nil
		}

		port, _ := command.Flags().GetInt("ssh-port")

		fmt.Println("Public IP: " + ip.String())

		user, _ := command.Flags().GetString("user")
		sshKey, _ := command.Flags().GetString("ssh-key")

		sshKeyPath := expandPath(sshKey)

		authMethod, closeSSHAgent, err := loadAuthMethod(sshKeyPath)
		if err != nil {
			return err
		}

		defer closeSSHAgent()

		config := &ssh.ClientConfig{
			User: user,
			Auth: []ssh.AuthMethod{
				authMethod,
			},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		}

		address := fmt.Sprintf("%s:%d", ip.String(), port)
		operator, err := operator.NewSSHOperator(address, config)

		if err != nil {
			return errors.Wrapf(err, "unable to connect to %s over ssh", address)
		}

		defer operator.Close()

		if !skipInstall {

			if printCommand {
				fmt.Printf("ssh: %s\n", installK3scommand)
			}

			res, err := operator.Execute(installK3scommand)

			if err != nil {
				return fmt.Errorf("error received processing command: %s", err)
			}

			fmt.Printf("Result: %s %s\n", string(res.StdOut), string(res.StdErr))
		}

		if printCommand {
			fmt.Printf("ssh: %s\n", getConfigcommand)
		}

		err = obtainKubeconfig(operator, getConfigcommand, ip.String(), context, localKubeconfig, merge)
		if err != nil {
			return err
		}

		return nil
	}

	command.PreRunE = func(command *cobra.Command, args []string) error {
		_, ipErr := command.Flags().GetIP("ip")
		if ipErr != nil {
			return ipErr
		}

		_, sshPortErr := command.Flags().GetInt("ssh-port")
		if sshPortErr != nil {
			return sshPortErr
		}
		return nil
	}
	return command
}

func obtainKubeconfig(operator operator.CommandOperator, getConfigcommand, ip, context, localKubeconfig string, merge bool) error {

	res, err := operator.Execute(getConfigcommand)

	if err != nil {
		return fmt.Errorf("error received processing command: %s", err)
	}

	fmt.Printf("Result: %s %s\n", string(res.StdOut), string(res.StdErr))

	absPath, _ := filepath.Abs(localKubeconfig)

	kubeconfig := rewriteKubeconfig(string(res.StdOut), ip, context)

	if merge {
		// Create a merged kubeconfig
		kubeconfig, err = mergeConfigs(absPath, []byte(kubeconfig))
		if err != nil {
			return err
		}
	}

	// Create a new kubeconfig
	if writeErr := writeConfig(absPath, []byte(kubeconfig), false); writeErr != nil {
		return writeErr
	}
	return nil
}

// Generates config files give the path to file: string and the data: []byte
func writeConfig(path string, data []byte, suppressMessage bool) error {
	absPath, _ := filepath.Abs(path)
	if !suppressMessage {
		fmt.Printf("Saving file to: %s\n", absPath)
		fmt.Printf("\n# Test your cluster with:\nexport KUBECONFIG=%s\nkubectl get node -o wide\n", absPath)
	}
	writeErr := ioutil.WriteFile(absPath, []byte(data), 0600)
	if writeErr != nil {
		return writeErr
	}
	return nil
}

func mergeConfigs(localKubeconfigPath string, k3sconfig []byte) ([]byte, error) {
	// Create a temporary kubeconfig to store the config of the newly create k3s cluster
	file, err := ioutil.TempFile(os.TempDir(), "k3s-temp-*")
	if err != nil {
		return nil, fmt.Errorf("Could not generate a temporary file to store the kuebeconfig: %s", err)
	}
	defer file.Close()

	if writeErr := writeConfig(file.Name(), []byte(k3sconfig), true); writeErr != nil {
		return nil, writeErr
	}

	fmt.Printf("Merging with existing kubeconfig at %s\n", localKubeconfigPath)

	// Append KUBECONFIGS in ENV Vars
	appendKubeConfigENV := fmt.Sprintf("KUBECONFIG=%s:%s", localKubeconfigPath, file.Name())

	// Merge the two kubeconfigs and read the output into 'data'
	cmd := exec.Command("kubectl", "config", "view", "--merge", "--flatten")
	cmd.Env = append(os.Environ(), appendKubeConfigENV)
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("Could not merge kubeconfigs: %s", err)
	}

	// Remove the temporarily generated file
	err = os.Remove(file.Name())
	if err != nil {
		return nil, errors.Wrapf(err, "Could not remove temporary kubeconfig file: %s", file.Name())
	}

	return data, nil
}

func expandPath(path string) string {
	res, _ := homedir.Expand(path)
	return res
}

func sshAgent(publicKeyPath string) (ssh.AuthMethod, func() error) {
	if sshAgentConn, err := net.Dial("unix", os.Getenv("SSH_AUTH_SOCK")); err == nil {
		sshAgent := agent.NewClient(sshAgentConn)

		keys, _ := sshAgent.List()
		if len(keys) == 0 {
			return nil, sshAgentConn.Close
		}

		pubkey, err := ioutil.ReadFile(publicKeyPath)
		if err != nil {
			return nil, sshAgentConn.Close
		}

		authkey, _, _, _, err := ssh.ParseAuthorizedKey(pubkey)
		if err != nil {
			return nil, sshAgentConn.Close
		}
		parsedkey := authkey.Marshal()

		for _, key := range keys {
			if bytes.Equal(key.Blob, parsedkey) {
				return ssh.PublicKeysCallback(sshAgent.Signers), sshAgentConn.Close
			}
		}
	}
	return nil, func() error { return nil }
}

func loadAuthMethod(privateKeyPath string) (ssh.AuthMethod, func() error, error) {
	noopCloseFunc := func() error { return nil }

	if privateKeyPath == "" {
		sshAgent, err := net.Dial("unix", os.Getenv("SSH_AUTH_SOCK"))

		if err != nil {
			return nil, noopCloseFunc, errors.Wrapf(err, "unable to reach SSH Agent")
		}

		return ssh.PublicKeysCallback(agent.NewClient(sshAgent).Signers), sshAgent.Close, nil
	}

	key, err := ioutil.ReadFile(privateKeyPath)
	if err != nil {
		return nil, noopCloseFunc, fmt.Errorf("unable to read file: %s, %s", privateKeyPath, err)
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		if _, ok := err.(*ssh.PassphraseMissingError); !ok {
			return nil, noopCloseFunc, fmt.Errorf("unable to parse private key: %s", err.Error())
		}

		agent, close := sshAgent(privateKeyPath + ".pub")
		if agent != nil {
			return agent, close, nil
		}

		defer close()

		fmt.Printf("Enter passphrase for '%s': ", privateKeyPath)
		STDIN := int(os.Stdin.Fd())
		bytePassword, _ := terminal.ReadPassword(STDIN)

		// Ignore any error from reading stdin to retain existing behaviour for unit test in
		// install_test.go

		// if err != nil {
		// 	return nil, noopCloseFunc, fmt.Errorf("reading password from stdin failed: %s", err.Error())
		// }

		fmt.Println()

		signer, err = ssh.ParsePrivateKeyWithPassphrase(key, bytePassword)
		if err != nil {
			return nil, noopCloseFunc, fmt.Errorf("parse private key with passphrase failed: %s", err)
		}
	}

	return ssh.PublicKeys(signer), noopCloseFunc, nil
}

func rewriteKubeconfig(kubeconfig string, ip string, context string) []byte {
	if context == "" {
		context = "default"
	}

	kubeconfigReplacer := strings.NewReplacer(
		"127.0.0.1", ip,
		"localhost", ip,
		"default", context,
	)

	return []byte(kubeconfigReplacer.Replace(kubeconfig))
}

func makeInstallExec(cluster bool, ip net.IP, tlsSAN string, options k3sExecOptions) string {
	extraArgs := []string{}
	if len(options.Datastore) > 0 {
		extraArgs = append(extraArgs, fmt.Sprintf("--datastore-endpoint %s", options.Datastore))
	}
	if options.FlannelIPSec {
		extraArgs = append(extraArgs, "--flannel-backend ipsec")
	}

	if options.NoExtras {
		extraArgs = append(extraArgs, "--no-deploy servicelb")
		extraArgs = append(extraArgs, "--no-deploy traefik")
	}

	extraArgs = append(extraArgs, options.ExtraArgs)
	extraArgsCmdline := ""
	for _, a := range extraArgs {
		extraArgsCmdline += a + " "
	}

	installExec := "INSTALL_K3S_EXEC='server"
	if cluster {
		installExec += " --cluster-init"
	}
	san := ip.String()
	if len(tlsSAN) > 0 {
		san = tlsSAN
	}
	installExec += fmt.Sprintf(" --tls-san %s", san)

	if trimmed := strings.TrimSpace(extraArgsCmdline); len(trimmed) > 0 {
		installExec += fmt.Sprintf(" %s", trimmed)
	}

	installExec += "'"

	return installExec
}
