package governance

import (
	"fmt"
	"github.com/boltdb/bolt"
	"github.com/golang/glog"
	"github.com/open-horizon/anax/citizenscientist"
	"github.com/open-horizon/anax/config"
	"github.com/open-horizon/anax/device"
	"github.com/open-horizon/anax/ethblockchain"
	"github.com/open-horizon/anax/events"
	"github.com/open-horizon/anax/exchange"
	"github.com/open-horizon/anax/persistence"
	"github.com/open-horizon/anax/policy"
	"github.com/open-horizon/anax/worker"
	"net/http"
	"runtime"
	"time"
)

// TODO: make this module more aware of long-running setup operations like torrent downloading and dockerfile loading
// the max time we'll let a contract remain unconfigured by the provider
const MAX_CONTRACT_UNCONFIGURED_TIME_M = 20

const MAX_CONTRACT_PRELAUNCH_TIME_M = 60

const MAX_MICROPAYMENT_UNPAID_RUN_DURATION_M = 60

// enforced only after the workloads are running
const MAX_AGREEMENT_ACCEPTANCE_WAIT_TIME_M = 20

// constants indicating why an agreement is cancelled
const CANCEL_NOT_FINALIZED_TIMEOUT = 100
const CANCEL_POLICY_CHANGED = 101
const CANCEL_TORRENT_FAILURE = 102
const CANCEL_CONTAINER_FAILURE = 103
const CANCEL_NOT_EXECUTED_TIMEOUT = 104
const CANCEL_USER_REQUESTED = 105
const CANCEL_DISCOVERED = 106

type GovernanceWorker struct {
	worker.Worker   // embedded field
	db              *bolt.DB
	bc              *ethblockchain.BaseContracts
	deviceId        string
	deviceToken     string
	pm              *policy.PolicyManager
	bcWritesEnabled bool // This field will be turned to true when the blockchain account has ether, which means
	// block chain writes (cancellations) can be done.
}

func NewGovernanceWorker(config *config.HorizonConfig, db *bolt.DB, pm *policy.PolicyManager) *GovernanceWorker {
	messages := make(chan events.Message)
	commands := make(chan worker.Command, 200)

	id, _ := device.Id()

	token := ""
	if dev, _ := persistence.FindExchangeDevice(db); dev != nil {
		token = dev.Token
	}

	worker := &GovernanceWorker{

		Worker: worker.Worker{
			Manager: worker.Manager{
				Config:   config,
				Messages: messages,
			},

			Commands: commands,
		},

		db:              db,
		pm:              pm,
		deviceId: id,
		deviceToken: token,
		bcWritesEnabled: false,
	}

	worker.start()
	return worker
}

func (w *GovernanceWorker) Messages() chan events.Message {
	return w.Worker.Manager.Messages
}

func (w *GovernanceWorker) NewEvent(incoming events.Message) {

	switch incoming.(type) {
	case *events.EdgeRegisteredExchangeMessage:
		msg, _ := incoming.(*events.EdgeRegisteredExchangeMessage)
		w.Commands <- NewDeviceRegisteredCommand(msg.Token())

	case *events.ContainerMessage:
		msg, _ := incoming.(*events.ContainerMessage)

		switch msg.Event().Id {
		case events.EXECUTION_BEGUN:
			glog.Infof("Begun execution of containers according to agreement %v", msg.AgreementId)

			cmd := w.NewStartGovernExecutionCommand(msg.Deployment, msg.AgreementProtocol, msg.AgreementId)
			w.Commands <- cmd
		case events.EXECUTION_FAILED:
			cmd := w.NewCleanupExecutionCommand(msg.AgreementProtocol, msg.AgreementId, CANCEL_CONTAINER_FAILURE, msg.Deployment)
			w.Commands <- cmd
		}

	case *events.TorrentMessage:
		msg, _ := incoming.(*events.TorrentMessage)
		switch msg.Event().Id {
		case events.TORRENT_FAILURE:
			cmd := w.NewCleanupExecutionCommand(msg.AgreementLaunchContext.AgreementProtocol, msg.AgreementLaunchContext.AgreementId, CANCEL_TORRENT_FAILURE, nil)
			w.Commands <- cmd
		}
	case *events.InitAgreementCancelationMessage:
		msg, _ := incoming.(*events.InitAgreementCancelationMessage)
		switch msg.Event().Id {
		case events.AGREEMENT_ENDED:
			cmd := w.NewCleanupExecutionCommand(msg.AgreementProtocol, msg.AgreementId, CANCEL_USER_REQUESTED, msg.Deployment)
			w.Commands <- cmd
		}
	case *events.ApiAgreementCancelationMessage:
		msg, _ := incoming.(*events.ApiAgreementCancelationMessage)
		switch msg.Event().Id {
		case events.AGREEMENT_ENDED:
			cmd := w.NewCleanupExecutionCommand(msg.AgreementProtocol, msg.AgreementId, CANCEL_USER_REQUESTED, msg.Deployment)
			w.Commands <- cmd
		}
	case *events.AccountFundedMessage:
		msg, _ := incoming.(*events.AccountFundedMessage)
		switch msg.Event().Id {
		case events.ACCOUNT_FUNDED:
			w.bcWritesEnabled = true
		}

	default: //nothing
	}

	return
}

func (w *GovernanceWorker) governAgreements() {

	// Establish the go objects that are used to interact with the ethereum blockchain.
	// This code should probably be in the protocol library.
	acct, _ := ethblockchain.AccountId()
	dir, _ := ethblockchain.DirectoryAddress()
	if bc, err := ethblockchain.InitBaseContracts(acct, w.Worker.Manager.Config.Edge.GethURL, dir); err != nil {
		glog.Errorf(logString(fmt.Sprintf("unable to initialize platform contracts, error: %v", err)))
		return
	} else {
		w.bc = bc
	}

	// go govern
	go func() {

		protocolHandler := citizenscientist.NewProtocolHandler(w.Config.Edge.GethURL, w.pm)

		for {
			glog.V(4).Infof(logString(fmt.Sprintf("governing pending agreements")))

			// Create a new filter for unfinalized agreements
			notYetFinalFilter := func() persistence.EAFilter {
				return func(a persistence.EstablishedAgreement) bool {
					return a.AgreementCreationTime != 0 && a.AgreementAcceptedTime != 0 && a.AgreementTerminatedTime == 0 && a.CounterPartyAddress != ""
				}
			}

			if establishedAgreements, err := persistence.FindEstablishedAgreements(w.db, citizenscientist.PROTOCOL_NAME, []persistence.EAFilter{persistence.UnarchivedEAFilter(),notYetFinalFilter()}); err != nil {
				glog.Errorf(logString(fmt.Sprintf("Unable to retrieve not yet final agreements from database: %v. Error: %v", err, err)))
			} else {

				// If there are agreemens in the database then we will assume that the device is already registered
				for _, ag := range establishedAgreements {
					if ag.AgreementFinalizedTime == 0 {
						// Verify that the blockchain update has occurred. If not, cancel the agreement.
						glog.V(5).Infof(logString(fmt.Sprintf("checking agreement %v for finalization.", ag.CurrentAgreementId)))
						if recorded, err := protocolHandler.VerifyAgreementRecorded(ag.CurrentAgreementId, ag.CounterPartyAddress, ag.ProposalSig, w.bc.Agreements); err != nil {
							glog.Errorf(logString(fmt.Sprintf("unable to verify agreement %v on blockchain, error: %v", ag.CurrentAgreementId, err)))
						} else if recorded {
							// Update state in the database
							if _, err := persistence.AgreementStateFinalized(w.db, ag.CurrentAgreementId, citizenscientist.PROTOCOL_NAME); err != nil {
								glog.Errorf(logString(fmt.Sprintf("error persisting agreement %v finalized: %v", ag.CurrentAgreementId, err)))
							}
							// Update state in exchange
							if proposal, err := protocolHandler.DemarshalProposal(ag.Proposal); err != nil {
								glog.Errorf(logString(fmt.Sprintf("could not hydrate proposal, error: %v", err)))
							} else if tcPolicy, err := policy.DemarshalPolicy(proposal.TsAndCs); err != nil {
								glog.Errorf(logString(fmt.Sprintf("error demarshalling TsAndCs policy for agreement %v, error %v", ag.CurrentAgreementId, err)))
							} else if err := recordProducerAgreementState(w.Config.Edge.ExchangeURL, w.deviceId, w.deviceToken, ag.CurrentAgreementId, tcPolicy.APISpecs[0].SpecRef, "Finalized Agreement"); err != nil {
								glog.Errorf(logString(fmt.Sprintf("error setting agreement %v finalized state in exchange: %v", ag.CurrentAgreementId, err)))
							}

						} else {
							glog.V(5).Infof(logString(fmt.Sprintf("detected agreement %v not yet final.", ag.CurrentAgreementId)))
							now := uint64(time.Now().Unix())
							if ag.AgreementCreationTime+w.Worker.Manager.Config.Edge.AgreementTimeoutS < now {
								// Start timing out the agreement
								glog.V(3).Infof(logString(fmt.Sprintf("detected agreement %v timed out.", ag.CurrentAgreementId)))

								w.cancelAgreement(ag.CurrentAgreementId, ag.AgreementProtocol, CANCEL_NOT_FINALIZED_TIMEOUT)

								// cleanup workloads
								w.Messages() <- events.NewGovernanceCancelationMessage(events.AGREEMENT_ENDED, events.AG_TERMINATED, ag.AgreementProtocol, ag.CurrentAgreementId, ag.CurrentDeployment)
							}
						}
					} else {

						// Make sure the agreement is still in the blockchain
						if recorded, err := protocolHandler.VerifyAgreementRecorded(ag.CurrentAgreementId, ag.CounterPartyAddress, ag.ProposalSig, w.bc.Agreements); err != nil {
							glog.Errorf(logString(fmt.Sprintf("unable to verify agreement %v on blockchain, error: %v", ag.CurrentAgreementId, err)))
						} else if !recorded {
							glog.Infof(logString(fmt.Sprintf("terminating agreement %v because it has been cancelled on the blockchain.", ag.CurrentAgreementId)))
							w.cancelAgreement(ag.CurrentAgreementId, ag.AgreementProtocol, CANCEL_DISCOVERED)
							// cleanup workloads if needed
							w.Messages() <- events.NewGovernanceCancelationMessage(events.AGREEMENT_ENDED, events.AG_TERMINATED, ag.AgreementProtocol, ag.CurrentAgreementId, ag.CurrentDeployment)
						}

						if ag.AgreementExecutionStartTime == 0 {
							// workload not started yet and in an agreement ...
							if (int64(ag.AgreementAcceptedTime) + (MAX_CONTRACT_PRELAUNCH_TIME_M * 60)) < time.Now().Unix() {
								glog.Infof(logString(fmt.Sprintf("terminating agreement %v because it hasn't been launched in max allowed time. This could be because of a workload failure.", ag.CurrentAgreementId)))
								w.cancelAgreement(ag.CurrentAgreementId, ag.AgreementProtocol, CANCEL_NOT_EXECUTED_TIMEOUT)
								// cleanup workloads if needed
								w.Messages() <- events.NewGovernanceCancelationMessage(events.AGREEMENT_ENDED, events.AG_TERMINATED, ag.AgreementProtocol, ag.CurrentAgreementId, ag.CurrentDeployment)
							}
						}
					}
				}
			}

			time.Sleep(10 * time.Second)
		}
	}()
}

func (w *GovernanceWorker) governContainers() {

	// go govern
	go func() {

		for {
			glog.V(4).Infof(logString(fmt.Sprintf("governing containers")))

			// Create a new filter for unfinalized agreements
			runningFilter := func() persistence.EAFilter {
				return func(a persistence.EstablishedAgreement) bool {
					return a.AgreementExecutionStartTime != 0 && a.AgreementTerminatedTime == 0 && a.CounterPartyAddress != ""
				}
			}

			if establishedAgreements, err := persistence.FindEstablishedAgreements(w.db, citizenscientist.PROTOCOL_NAME, []persistence.EAFilter{persistence.UnarchivedEAFilter(),runningFilter()}); err != nil {
				glog.Errorf(logString(fmt.Sprintf("Unable to retrieve running agreements from database, error: %v", err)))
			} else {

				for _, ag := range establishedAgreements {

					// Make sure containers are still running.
					glog.V(3).Infof(logString(fmt.Sprintf("fire event to ensure containers are still up for agreement %v.", ag.CurrentAgreementId)))

					// current contract, ensure workloads still running
					w.Messages() <- events.NewGovernanceMaintenanceMessage(events.CONTAINER_MAINTAIN, ag.AgreementProtocol, ag.CurrentAgreementId, ag.CurrentDeployment)

				}
			}

			time.Sleep(1 * time.Minute)
		}
	}()
}

// It cancels the given agreement. Please take note that the system is very asynchronous. It is
// possible for multiple cancellations to occur in the time it takes to actually stop workloads and
// cancel on the blockchain, therefore this code needs to be prepared to run multiple times for the
// same agreement id.
func (w *GovernanceWorker) cancelAgreement(agreementId string, agreementProtocol string, reason uint) {
	protocolHandler := citizenscientist.NewProtocolHandler(w.Config.Edge.GethURL, w.pm)

	// Update the database
	var ag *persistence.EstablishedAgreement
	if agreement, err := persistence.AgreementStateTerminated(w.db, agreementId, agreementProtocol); err != nil {
		glog.Errorf(logString(fmt.Sprintf("error marking agreement %v terminated: %v", agreementId, err)))
	} else {
		ag = agreement
	}

	// Delete from the exchange
	if err := deleteProducerAgreement(w.Config.Edge.ExchangeURL, w.deviceId, w.deviceToken, agreementId); err != nil {
		glog.Errorf(logString(fmt.Sprintf("error deleting agreement %v in exchange: %v", agreementId, err)))
	}

	// Get the policy we used in the agreement and then cancel on the blockchain
	glog.V(5).Infof(logString(fmt.Sprintf("terminating agreement %v on blockchain.", agreementId)))

	if ag != nil {
		if proposal, err := protocolHandler.DemarshalProposal(ag.Proposal); err != nil {
			glog.Errorf(logString(fmt.Sprintf("error demarshalling agreement %v proposal: %v", agreementId, err)))
		} else if pPolicy, err := policy.DemarshalPolicy(proposal.ProducerPolicy); err != nil {
			glog.Errorf(logString(fmt.Sprintf("error demarshalling agreement %v Producer Policy: %v", agreementId, err)))
		} else if err := protocolHandler.TerminateAgreement(pPolicy, ag.CounterPartyAddress, agreementId, reason, w.bc.Agreements); err != nil {
			glog.Errorf(logString(fmt.Sprintf("error terminating agreement %v on the blockchain: %v", agreementId, err)))
		}
	}

	// Archive
	glog.V(5).Infof(logString(fmt.Sprintf("archiving agreement %v", agreementId)))
	if _, err := persistence.ArchiveEstablishedAgreement(w.db, agreementId, agreementProtocol); err != nil {
		glog.Errorf(logString(fmt.Sprintf("error archiving terminated agreement: %v, error: %v", agreementId, err)))
	}
}

func (w *GovernanceWorker) start() {
	go func() {

		// Hold the governance functions until we have blockchain funding. If there are events occurring that
		// we need to react to, they will queue up on the command queue while we wait here. The agreement worker
		// should not be blocked by this.
		for {
			if w.bcWritesEnabled == false {
				time.Sleep(time.Duration(5) * time.Second)
				glog.V(3).Infof("GovernanceWorker command processor waiting for funding")
			} else {
				break
			}
		}

		// Fire up the agreement governor and the container governor
		w.governAgreements()
		w.governContainers()

		// Fire up the command processor
		for {

			glog.V(4).Infof("GovernanceWorker command processor blocking waiting to receive incoming commands")

			command := <-w.Commands
			glog.V(2).Infof("GovernanceWorker received command: %v", command)

			// TODO: consolidate DB update cases
			switch command.(type) {
			case *DeviceRegisteredCommand:
				cmd, _ := command.(*DeviceRegisteredCommand)
				w.deviceToken = cmd.Token

			case *StartGovernExecutionCommand:
				// TODO: update db start time and tc so it can be governed
				cmd, _ := command.(*StartGovernExecutionCommand)
				glog.V(3).Infof("Starting governance on resources in agreement: %v", cmd.AgreementId)

				if _, err := persistence.AgreementStateExecutionStarted(w.db, cmd.AgreementId, cmd.AgreementProtocol, &cmd.Deployment); err != nil {
					glog.Errorf("Failed to update local contract record to start governing Agreement: %v. Error: %v", cmd.AgreementId, err)
				}

			case *CleanupExecutionCommand:
				cmd, _ := command.(*CleanupExecutionCommand)
				glog.V(3).Infof("Ending the agreement: %v", cmd.AgreementId)

				w.cancelAgreement(cmd.AgreementId, cmd.AgreementProtocol, cmd.Reason)

				// send the event to the container in case it has started the workloads.
				w.Messages() <- events.NewGovernanceCancelationMessage(events.AGREEMENT_ENDED, events.AG_TERMINATED, cmd.AgreementProtocol, cmd.AgreementId, cmd.Deployment)
			}

			runtime.Gosched()
		}
	}()
}

// TODO: consolidate below
type StartGovernExecutionCommand struct {
	AgreementId       string
	AgreementProtocol string
	Deployment        map[string]persistence.ServiceConfig
}

func (w *GovernanceWorker) NewStartGovernExecutionCommand(deployment map[string]persistence.ServiceConfig, protocol string, agreementId string) *StartGovernExecutionCommand {
	return &StartGovernExecutionCommand{
		AgreementId:       agreementId,
		AgreementProtocol: protocol,
		Deployment:        deployment,
	}
}

type CleanupExecutionCommand struct {
	AgreementProtocol string
	AgreementId       string
	Reason            uint
	Deployment        map[string]persistence.ServiceConfig
}

func (w *GovernanceWorker) NewCleanupExecutionCommand(protocol string, agreementId string, reason uint, deployment map[string]persistence.ServiceConfig) *CleanupExecutionCommand {
	return &CleanupExecutionCommand{
		AgreementProtocol: protocol,
		AgreementId:       agreementId,
		Reason:            reason,
		Deployment:        deployment,
	}
}

type DeviceRegisteredCommand struct {
	Token string
}

func NewDeviceRegisteredCommand(token string) *DeviceRegisteredCommand {
	return &DeviceRegisteredCommand{
		Token: token,
	}
}

func recordProducerAgreementState(url string, deviceId string, token string, agreementId string, microservice string, state string) error {

	glog.V(5).Infof(logString(fmt.Sprintf("setting agreement %v state to %v", agreementId, state)))

	as := new(exchange.PutAgreementState)
	as.Microservice = microservice
	as.State = state
	var resp interface{}
	resp = new(exchange.PostDeviceResponse)
	targetURL := url + "devices/" + deviceId + "/agreements/" + agreementId
	for {
		if err, tpErr := exchange.InvokeExchange(&http.Client{}, "PUT", targetURL, deviceId, token, &as, &resp); err != nil {
			glog.Errorf(logString(fmt.Sprintf(err.Error())))
			return err
		} else if tpErr != nil {
			glog.Warningf(err.Error())
			time.Sleep(10 * time.Second)
			continue
		} else {
			glog.V(5).Infof(logString(fmt.Sprintf("set agreement %v to state %v", agreementId, state)))
			return nil
		}
	}

}

func deleteProducerAgreement(url string, deviceId string, token string, agreementId string) error {

	glog.V(5).Infof(logString(fmt.Sprintf("deleting agreement %v in exchange", agreementId)))

	var resp interface{}
	resp = new(exchange.PostDeviceResponse)
	targetURL := url + "devices/" + deviceId + "/agreements/" + agreementId
	for {
		if err, tpErr := exchange.InvokeExchange(&http.Client{}, "DELETE", targetURL, deviceId, token, nil, &resp); err != nil {
			glog.Errorf(logString(fmt.Sprintf(err.Error())))
			return err
		} else if tpErr != nil {
			glog.Warningf(err.Error())
			time.Sleep(10 * time.Second)
			continue
		} else {
			glog.V(5).Infof(logString(fmt.Sprintf("deleted agreement %v from exchange", agreementId)))
			return nil
		}
	}

}

var logString = func(v interface{}) string {
	return fmt.Sprintf("GovernanceWorker: %v", v)
}
