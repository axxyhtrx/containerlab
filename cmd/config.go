package cmd

import (
	"fmt"
	"sync"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/srl-labs/containerlab/clab"
	"github.com/srl-labs/containerlab/clab/config"
	"golang.org/x/crypto/ssh"
)

// path to additional templates
var templatePath string

// Only print config locally, dont send to the node
var printLines int

// configCmd represents the config command
var configCmd = &cobra.Command{
	Use:          "config",
	Short:        "configure a lab",
	Long:         "configure a lab based using templates and variables from the topology definition file\nreference: https://containerlab.srlinux.dev/cmd/config/",
	Aliases:      []string{"conf"},
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		var err error
		if err = topoSet(); err != nil {
			return err
		}

		opts := []clab.ClabOption{
			clab.WithDebug(debug),
			clab.WithTimeout(timeout),
			clab.WithTopoFile(topo),
			clab.WithEnvDockerClient(),
		}
		c := clab.NewContainerLab(opts...)

		//ctx, cancel := context.WithCancel(context.Background())
		//defer cancel()

		setFlags(c.Config)
		log.Debugf("lab Conf: %+v", c.Config)
		// Parse topology information
		if err = c.ParseTopology(); err != nil {
			return err
		}

		// config map per node. each node gets a couple of config snippets []string
		allConfig := make(map[string][]config.ConfigSnippet)

		renderErr := 0

		for _, node := range c.Nodes {
			kind := node.Labels["clab-node-kind"]
			err = config.LoadTemplate(kind, templatePath)
			if err != nil {
				return err
			}

			res, err := config.RenderNode(node)
			if err != nil {
				log.Errorln(err)
				renderErr += 1
				continue
			}
			allConfig[node.LongName] = append(allConfig[node.LongName], res...)

		}

		for lIdx, link := range c.Links {

			res, err := config.RenderLink(link)
			if err != nil {
				log.Errorf("%d. %s\n", lIdx, err)
				renderErr += 1
				continue
			}
			for _, rr := range res {
				allConfig[rr.TargetNode.LongName] = append(allConfig[rr.TargetNode.LongName], rr)
			}

		}

		if renderErr > 0 {
			return fmt.Errorf("%d render warnings", renderErr)
		}

		if printLines > 0 {
			// Debug log all config to be deployed
			for _, v := range allConfig {
				for _, r := range v {
					r.Print(printLines)
				}
			}
			return nil
		}

		var wg sync.WaitGroup
		wg.Add(len(allConfig))
		for _, cs_ := range allConfig {
			go func(cs []config.ConfigSnippet) {
				defer wg.Done()

				var transport config.Transport

				ct, ok := cs[0].TargetNode.Labels["config.transport"]
				if !ok {
					ct = "ssh"
				}

				if ct == "ssh" {
					transport, _ = newSSHTransport(cs[0].TargetNode)
					if err != nil {
						log.Errorf("%s: %s", kind, err)
					}
				} else if ct == "grpc" {
					// newGRPCTransport
				} else {
					log.Errorf("Unknown transport: %s", ct)
					return
				}

				err := config.WriteConfig(transport, cs)
				if err != nil {
					log.Errorf("%s\n", err)
				}

			}(cs_)
		}
		wg.Wait()

		return nil
	},
}

func newSSHTransport(node *clab.Node) (*config.SshTransport, error) {
	switch node.Kind {
	case "vr-sros", "srl":
		c := &config.SshTransport{}
		c.SshConfig = &ssh.ClientConfig{}
		config.SshConfigWithUserNamePassword(
			c.SshConfig,
			clab.DefaultCredentials[node.Kind][0],
			clab.DefaultCredentials[node.Kind][1])

		switch node.Kind {
		case "vr-sros":
			c.K = &config.VrSrosSshKind{}
		case "srl":
			c.K = &config.SrlSshKind{}
		}
		return c, nil
	}
	return nil, fmt.Errorf("no tranport implemented for kind: %s", kind)
}

func init() {
	rootCmd.AddCommand(configCmd)
	configCmd.Flags().StringVarP(&templatePath, "path", "", "", "specify template path")
	configCmd.MarkFlagDirname("path")
	configCmd.Flags().StringVarP(&config.TemplateOverride, "templates", "", "", "specify a list of template to apply")
	configCmd.Flags().IntVarP(&printLines, "print-only", "p", 0, "print config, don't send it. Restricted to n lines")
	configCmd.Flags().BoolVarP(&config.LoginMessages, "login-message", "", false, "show the SSH login message")
}
