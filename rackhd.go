package rackhd

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net"
	"strconv"
	"strings"

	apiclient "github.com/emccode/gorackhd/client"
	"github.com/emccode/gorackhd/client/lookups"

	httptransport "github.com/go-swagger/go-swagger/httpkit/client"
	"github.com/go-swagger/go-swagger/strfmt"

	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/log"
	"github.com/docker/machine/libmachine/mcnflag"
	"github.com/docker/machine/libmachine/ssh"
	"github.com/docker/machine/libmachine/state"

	cryptossh "golang.org/x/crypto/ssh"
)

type Driver struct {
	*drivers.BaseDriver
	Endpoint    string
	NodeID      string
	SSHUser     string
	SSHPassword string
	SSHPort     int
	SSHKey      string
	Transport   string
	client      *apiclient.Monorail
}

const (
	defaultEndpoint    = "localhost:8080"
	defaultTransport   = "http"
	defaultSSHUser     = "root"
	defaultSSHPassword = "root"
	defaultSSHPort     = 22
)

func (d *Driver) GetCreateFlags() []mcnflag.Flag {
	return []mcnflag.Flag{
		mcnflag.StringFlag{
			EnvVar: "RACKHD_ENDPOINT",
			Name:   "rackhd-endpoint",
			Usage:  "RackHD Endpoint for API traffic",
			Value:  defaultEndpoint,
		},
		mcnflag.StringFlag{
			EnvVar: "RACKHD_NODE_ID",
			Name:   "rackhd-node-id",
			Usage:  "REQUIRED: Specify Node ID, MAC Address or IP Address",
		},
		mcnflag.StringFlag{
			EnvVar: "RACKHD_TRANSPORT",
			Name:   "rackhd-transport",
			Usage:  "RackHD Endpoint Transport. Specify http or https. HTTP is default",
			Value:  defaultTransport,
		},
		mcnflag.StringFlag{
			EnvVar: "RACKHD_SSH_USER",
			Name:   "rackhd-ssh-user",
			Usage:  "ssh user (default:root)",
			Value:  defaultSSHUser,
		},
		mcnflag.StringFlag{
			EnvVar: "RACKHD_SSH_PASSWORD",
			Name:   "rackhd-ssh-password",
			Usage:  "ssh password (default:root)",
			Value:  defaultSSHPassword,
		},
		mcnflag.IntFlag{
			EnvVar: "RACKHD_SSH_PORT",
			Name:   "rackhd-ssh-port",
			Usage:  "ssh port (default:22)",
			Value:  defaultSSHPort,
		},
		/*
			TODO: Grab SSH User and PW from Workflow.
			mcnflag.StringFlag{
				EnvVar: "RACKHD_WORKFLOW_ID",
				Name:   "rackhd-workflow-id",
				Usage:  "workflow ID used to extract SSH user information (optional)",
			},
			TODO: Implicit creation from a pool
			mcnflag.StringFlag{
				EnvVar: "RACKHD_POOL_ID",
				Name:   "rackhd-POOL-id",
				Usage:  "POOL ID",
			},
			TODO: API Authentication Values. Will be detemined for v 2.0 of API
			mcnflag.StringFlag{
				EnvVar: "RACKHD_ENDPOINT_AUTH",
				Name:   "rackhd-ENDPOINT_AUTH,
				Usage:  "ENDPOINT_AUTH",
			},
		*/
	}
}

func NewDriver(hostName, storePath string) *Driver {
	return &Driver{
		Endpoint:    defaultEndpoint,
		SSHPassword: defaultSSHPassword,
		Transport:   defaultTransport,
		BaseDriver: &drivers.BaseDriver{
			SSHUser:     defaultSSHUser,
			SSHPort:     defaultSSHPort,
			MachineName: hostName,
			StorePath:   storePath,
		},
	}
}

func (d *Driver) GetMachineName() string {
	return d.MachineName
}

func (d *Driver) DriverName() string {
	return "rackhd"
}

func (d *Driver) SetConfigFromFlags(flags drivers.DriverOptions) error {
	d.Endpoint = flags.String("rackhd-endpoint")

	d.NodeID = flags.String("rackhd-node-id")
	if d.NodeID == "" {
		return fmt.Errorf("rackhd driver requires the --rackhd-node-id option")
	}

	d.SSHUser = flags.String("rackhd-ssh-user")
	d.SSHPassword = flags.String("rackhd-ssh-password")
	d.SSHPort = flags.Int("rackhd-ssh-port")
	if d.SSHPort == 443 {
		d.Transport = "https"
	} else {
		d.Transport = flags.String("rackhd-transport")
	}

	return nil
}

func (d *Driver) PreCreateCheck() error {
	log.Infof("Testing accessibility of endpoint: %v", d.Endpoint)
	//Generate the client
	client := d.getClient()

	//do a test to see if the server is available. 2nd Nil is authentication params
	// that need to be determined in v2.0 of API
	_, err := client.Config.GetConfig(nil, nil)
	if err != nil {
		return fmt.Errorf("The Endpoint is not accessible. Error: %s", err)
	}
	log.Infof("Test Passed. %v is accessbile and installation will begin", d.Endpoint)
	return nil
}

func (d *Driver) Create() error {
	//Generate the client
	client := d.getClient()

	// do a lookup on the ID to retrieve IP information
	resp, err := client.Lookups.GetLookups(&lookups.GetLookupsParams{Q: d.NodeID}, nil)
	if err != nil {
		return err
	}

	// new slice for all IP addresses found for the node
	ipAddSlice := make([]string, 0)

	//loop through the response and grab all the IP addresses
	for _, v := range resp.Payload {
		if rec, ok := v.(map[string]interface{}); ok {
			for key, val := range rec {
				if key == "ipAddress" {
					log.Debugf("Found IP Address for Node ID: %v", val.(string))
					ipAddSlice = append(ipAddSlice, val.(string))
				}
			}
		}
	}

	//if the slice is empty that means there are no IPs
	if len(ipAddSlice) <= 0 {
		return fmt.Errorf("No IP addresses are associated with the Node ID specified. Error: %s", err)
	}

	// loop through slice and see if we can connect to the ip:ssh-port
	for _, ipAddy := range ipAddSlice {
		ipPort := ipAddy + ":" + strconv.Itoa(d.SSHPort)
		log.Debugf("Testing connection to: %v", ipPort)
		conn, err := net.DialTimeout("tcp", ipPort, 25000000000)
		if err != nil {
			log.Debugf("Connection failed on: %v", ipPort)
		} else {
			log.Infof("Connection succeeded on: %v", ipPort)
			d.IPAddress = string(ipAddy)
			conn.Close()
			break
		}
	}

	if d.IPAddress == "" {
		return fmt.Errorf("No IP addresses are accessible on this network to the Node ID specified. Error: %s", err)
	}

	//create public SSH key
	log.Infof("Creating SSH key...")
	key, err := d.createSSHKey()
	if err != nil {
		return err
	}
	d.SSHKey = strings.TrimSpace(key)

	//TAKEN FROM THE FUSION DRIVER TO USE SSH [THANKS!]
	log.Infof("Copy public SSH key to %s [%s]", d.MachineName, d.IPAddress)
	// create .ssh folder in users home
	if err := executeSSHCommand(fmt.Sprintf("mkdir -p /home/%s/.ssh", d.SSHUser), d); err != nil {
		return err
	}
	// add public ssh key to authorized_keys
	if err := executeSSHCommand(fmt.Sprintf("echo '%v' > /home/%s/.ssh/authorized_keys", d.SSHKey, d.SSHUser), d); err != nil {
		return err
	}
	// make it secure
	if err := executeSSHCommand(fmt.Sprintf("chmod 700 /home/%s/.ssh", d.SSHUser), d); err != nil {
		return err
	}
	// make it secure
	if err := executeSSHCommand(fmt.Sprintf("chmod 600 /home/%s/.ssh/authorized_keys", d.SSHUser), d); err != nil {
		return err
	}

	return nil
}

func (d *Driver) GetSSHHostname() (string, error) {
	return d.GetIP()
}

func (d *Driver) GetSSHUsername() string {
	if d.SSHUser == "" {
		d.SSHUser = "root"
	}
	return d.SSHUser
}

func (d *Driver) createSSHKey() (string, error) {
	if err := ssh.GenerateSSHKey(d.GetSSHKeyPath()); err != nil {
		return "", err
	}

	publicKey, err := ioutil.ReadFile(d.publicSSHKeyPath())
	if err != nil {
		return "", err
	}

	return string(publicKey), nil
}

func (d *Driver) GetURL() (string, error) {
	ip, err := d.GetIP()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("tcp://%s:2376", ip), nil
}

func (d *Driver) GetIP() (string, error) {
	if d.IPAddress == "" {
		return "", fmt.Errorf("IP address is not set")
	}
	return d.IPAddress, nil
}

func (d *Driver) GetState() (state.State, error) {
	/*
		TODO: THIS REQUIRES THE REDFISH API WHICH IS STILL IN DEVELOPMENT
		switch instance.State {
		case "online":
			return state.Running, nil
		case "offline":
			return state.Stopped, nil
		}
		return state.None, nil
	*/
	return state.Running, nil
}

func (d *Driver) Start() error {
	/*
		TODO: THIS REQUIRES THE REDFISH API WHICH IS STILL IN DEVELOPMENT
		REMOTELY POWER ON A SERVER VIA IPMI
	*/
	return nil
}

func (d *Driver) Stop() error {
	/*
		TODO: THIS REQUIRES THE REDFISH API WHICH IS STILL IN DEVELOPMENT
		SEND A SIGKILL TO THE OS. OR USE THE API TO GRACEFULLY SHUTDOWN THE HOST
	*/
	return nil
}

func (d *Driver) Remove() error {
	/*
		TODO: DECIDE WHETHER TO UNINSTALL DOCKER OR
		1. ADD A GENERIC WORKFLOW
		2. REBOOT THE HOST
		3. HOPE THAT GENERIC WORKFLOW WILL RESET THE HOST BACK TO A BLANK SLATE
	*/
	return nil
}

func (d *Driver) Restart() error {
	/*
		TODO: THIS REQUIRES THE REDFISH API WHICH IS STILL IN DEVELOPMENT
		REMOTELY RESET OFF A SERVER VIA IPMI
	*/
	return nil
}

func (d *Driver) Kill() error {
	/*
		TODO: THIS REQUIRES THE REDFISH API WHICH IS STILL IN DEVELOPMENT
		POWER OFF THE HOST VIA IMPI
	*/
	return nil
}

func (d *Driver) getClient() *apiclient.Monorail {
	log.Debugf("Getting RackHD Client")
	if d.client == nil {
		// create the transport
		/** Will Need to determine changes for v 2.0 API **/
		transport := httptransport.New(d.Endpoint, "/api/1.1", []string{d.Transport})
		// create the API client, with the transport
		d.client = apiclient.New(transport, strfmt.Default)
	}
	return d.client
}

func (d *Driver) publicSSHKeyPath() string {
	return d.GetSSHKeyPath() + ".pub"
}

// execute command over SSH with user / password authentication
func executeSSHCommand(command string, d *Driver) error {
	log.Debugf("Execute executeSSHCommand: %s", command)

	config := &cryptossh.ClientConfig{
		User: d.SSHUser,
		Auth: []cryptossh.AuthMethod{
			cryptossh.Password(d.SSHPassword),
		},
	}

	client, err := cryptossh.Dial("tcp", fmt.Sprintf("%s:%d", d.IPAddress, d.SSHPort), config)
	if err != nil {
		log.Debugf("Failed to dial:", err)
		return err
	}

	session, err := client.NewSession()
	if err != nil {
		log.Debugf("Failed to create session: " + err.Error())
		return err
	}
	defer session.Close()

	var b bytes.Buffer
	session.Stdout = &b

	if err := session.Run(command); err != nil {
		log.Debugf("Failed to run: " + err.Error())
		return err
	}
	log.Debugf("Stdout from executeSSHCommand: %s", b.String())

	return nil
}
