package common

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2instanceconnect"
	"github.com/aws/aws-sdk-go/service/ssm"
	pssm "github.com/hashicorp/packer-plugin-amazon/builder/common/ssm"
	"github.com/hashicorp/packer-plugin-sdk/communicator"
	"github.com/hashicorp/packer-plugin-sdk/communicator/sshkey"
	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/net"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
)

type StepCreateSSMTunnel struct {
	AWSSession       *session.Session
	Region           string
	LocalPortNumber  int
	RemotePortNumber int
	SSMAgentEnabled  bool
	SSHConfig        *communicator.SSH
	PauseBeforeSSM   time.Duration
	stopSSMCommand   func()
}

// Run executes the Packer build step that creates a session tunnel.
func (s *StepCreateSSMTunnel) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	ui := state.Get("ui").(packersdk.Ui)

	if !s.SSMAgentEnabled {
		return multistep.ActionContinue
	}

	// Wait for the remote port to become available
	if s.PauseBeforeSSM > 0 {
		ui.Say(fmt.Sprintf("Waiting %s before establishing the SSM session...", s.PauseBeforeSSM))
		select {
		case <-time.After(s.PauseBeforeSSM):
			break
		case <-ctx.Done():
			return multistep.ActionHalt
		}
	}

	// Configure local port number
	if err := s.ConfigureLocalHostPort(ctx); err != nil {
		err := fmt.Errorf("error finding an available port to initiate a session tunnel: %s", err)
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}

	// Get instance information
	instance, ok := state.Get("instance").(*ec2.Instance)
	if !ok {
		err := fmt.Errorf("error encountered in obtaining target instance id for session tunnel")
		ui.Error(err.Error())
		state.Put("error", err)
		return multistep.ActionHalt
	}

	state.Put("sessionPort", s.LocalPortNumber)

	ssmCtx, ssmCancel := context.WithCancel(ctx)
	s.stopSSMCommand = ssmCancel
	ec2Conn := state.Get("ec2").(*ec2.EC2)

	ssmconn := ssm.New(s.AWSSession)
	session := pssm.Session{
		SvcClient:  ssmconn,
		InstanceID: aws.StringValue(instance.InstanceId),
		RemotePort: s.RemotePortNumber,
		LocalPort:  s.LocalPortNumber,
		Region:     s.Region,
		Ec2Conn:    ec2Conn,
	}
	go s.CreatePersistentSSMSession(ssmCtx, ui, &session, instance)

	return multistep.ActionContinue
}

func (s *StepCreateSSMTunnel) CreatePersistentSSMSession(ctx context.Context, ui packersdk.Ui, session *pssm.Session, instance *ec2.Instance) {
	sessionChan := make(chan struct{})

	go func() {
		// SSH public key sent expires every minute.
		// Send it upon each reconnect to ensure it is always valid.
		for range sessionChan {
			if len(s.SSHConfig.SSHPrivateKey) != 0 && s.SSHConfig.SSHKeyPairName == "" {
				ui.Say("Uploading SSH public key to instance")
				err := s.sendUserSSHPublicKey(instance, s.SSHConfig.SSHPrivateKey)
				if err != nil {
					ui.Error(err.Error())
				}
			}
		}
	}()

	err := session.Start(ctx, ui, sessionChan)
	if err != nil {
		ui.Error(fmt.Sprintf("ssm error: %s", err))
	}
}

func (s *StepCreateSSMTunnel) sendUserSSHPublicKey(
	instance *ec2.Instance,
	privateKey []byte,
) error {
	publicKey, err := sshkey.PublicKeyFromPrivate(privateKey)
	if err != nil {
		return fmt.Errorf("Error getting public key from private key: %s", err)
	}
	svc := ec2instanceconnect.New(s.AWSSession)
	input := &ec2instanceconnect.SendSSHPublicKeyInput{
		AvailabilityZone: aws.String(*instance.Placement.AvailabilityZone),
		InstanceId:       aws.String(*instance.InstanceId),
		InstanceOSUser:   aws.String(s.SSHConfig.SSHUsername),
		SSHPublicKey:     aws.String(strings.TrimSuffix(string(publicKey), "\n")),
	}
	log.Printf("Sending public key to instance: %s", *input.InstanceId)
	result, err := svc.SendSSHPublicKey(input)
	if err != nil {
		err := fmt.Errorf(`
        error encountered in sending public key to instance: %s
      Check the key type and length are valid in AWS API.
      https://docs.aws.amazon.com/ec2-instance-connect/latest/APIReference/API_SendSSHPublicKey.html`, err)
		return err
	} else {
		if *result.Success {
			return nil
		}
	}
	return fmt.Errorf("Failed to send public key to instance")
}

// Cleanup terminates an active session on AWS, which in turn terminates the associated tunnel process running on the local machine.
func (s *StepCreateSSMTunnel) Cleanup(state multistep.StateBag) {
	if !s.SSMAgentEnabled {
		return
	}

	if s.stopSSMCommand != nil {
		s.stopSSMCommand()
	}
}

// ConfigureLocalHostPort finds an available port on the localhost that can be used for the remote tunnel.
// Defaults to using s.LocalPortNumber if it is set.
func (s *StepCreateSSMTunnel) ConfigureLocalHostPort(ctx context.Context) error {
	minPortNumber, maxPortNumber := 8000, 9000

	if s.LocalPortNumber != 0 {
		minPortNumber = s.LocalPortNumber
		maxPortNumber = minPortNumber
	}

	// Find an available TCP port for our HTTP server
	l, err := net.ListenRangeConfig{
		Min:     minPortNumber,
		Max:     maxPortNumber,
		Addr:    "0.0.0.0",
		Network: "tcp",
	}.Listen(ctx)
	if err != nil {
		return err
	}

	s.LocalPortNumber = l.Port
	// Stop listening on selected port so that the AWS session-manager-plugin can use it.
	// The port is closed right before we start the session to avoid two Packer builds from getting the same port - fingers-crossed
	l.Close()

	return nil

}
