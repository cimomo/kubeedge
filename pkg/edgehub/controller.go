package edgehub

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"sync"
	"time"

	"github.com/kubeedge/kubeedge/beehive/pkg/common/log"
	"github.com/kubeedge/kubeedge/beehive/pkg/core"
	"github.com/kubeedge/kubeedge/beehive/pkg/core/context"
	"github.com/kubeedge/kubeedge/beehive/pkg/core/model"

	"github.com/kubeedge/kubeedge/pkg/common/message"
	"github.com/kubeedge/kubeedge/pkg/edgehub/clients"
	http_utils "github.com/kubeedge/kubeedge/pkg/edgehub/common/http"
	"github.com/kubeedge/kubeedge/pkg/edgehub/config"
)

const (
	waitConnectionPeriod = time.Minute
)

var (
	authEventType = "auth_info_event"
	groupMap      = map[string]string{"resource": core.MetaGroup,
		"twin": core.TwinGroup, "app": "sync",
		"func": core.MetaGroup, "user": core.BusGroup}

	// clear the number of data of the stop channel
	times = 2
)

type EdgeHubController struct {
	context    *context.Context
	chClient   clients.Adapter
	config     *config.ControllerConfig
	stopChan   chan struct{}
	syncKeeper map[string]chan model.Message
	keeperLock sync.RWMutex
}

func NewEdgeHubController() *EdgeHubController {
	return &EdgeHubController{
		config:     &config.GetConfig().CtrConfig,
		stopChan:   make(chan struct{}),
		syncKeeper: make(map[string]chan model.Message),
	}
}

func (ehc *EdgeHubController) initial(ctx *context.Context) error {
	getUrl := func() string {
		for {
			url, err := ehc.getCloudHubUrl()
			if err != nil {
				log.LOGGER.Warnf("failed to get cloud hub url, error:%+v", err)
				time.Sleep(time.Minute)
				continue
			}
			return url
		}
	}

	if ehc.config.ProjectID != "" && ehc.config.NodeId != "" {
		cloudHubUrl := getUrl()
		// TODO: set url gracefully
		config.GetConfig().WSConfig.Url = cloudHubUrl
	} else {
		log.LOGGER.Warnf("use the config url for testing")
	}

	cloudHubClient := clients.GetClient(clients.ClientTypeWebSocket, config.GetConfig())
	if cloudHubClient == nil {
		log.LOGGER.Errorf("failed to get web socket client")
		return fmt.Errorf("failed to get web socket client")
	}

	ehc.context = ctx
	ehc.chClient = cloudHubClient

	return nil
}

func (ehc *EdgeHubController) Start(ctx *context.Context) {
	for {
		err := ehc.initial(ctx)
		if err != nil {
			log.LOGGER.Fatalf("failed to init controller: %v", err)
			return
		}

		err = ehc.chClient.Init()
		if err != nil {
			log.LOGGER.Errorf("connection error, try again after 60s: %v", err)
			time.Sleep(waitConnectionPeriod)
			continue
		}

		// execute hook func after connect
		ehc.pubConnectInfo(true)

		go ehc.routeToEdge()
		go ehc.routeToCloud()
		go ehc.keepalive()

		// wait the stop singal
		// stop authinfo manager/websocket connection
		<-ehc.stopChan
		ehc.chClient.Uninit()

		// execute hook fun after disconnect
		ehc.pubConnectInfo(false)

		// sleep one period of heartbeat, then try to connect cloud hub again
		time.Sleep(ehc.config.HeartbeatPeroid * 2)

		// clean channel
		for i := 0; i < times; i++ {
			select {
			case <-ehc.stopChan:
				continue
			default:
			}
		}
	}
}

func (ehc *EdgeHubController) addKeepChannel(msgID string) chan model.Message {
	ehc.keeperLock.Lock()
	defer ehc.keeperLock.Unlock()

	tempChannel := make(chan model.Message)
	ehc.syncKeeper[msgID] = tempChannel

	return tempChannel
}

func (ehc *EdgeHubController) deleteKeepChannel(msgID string) {
	ehc.keeperLock.Lock()
	defer ehc.keeperLock.Unlock()

	delete(ehc.syncKeeper, msgID)
}

func (ehc *EdgeHubController) isSyncResponse(msgID string) bool {
	ehc.keeperLock.RLock()
	defer ehc.keeperLock.RUnlock()

	_, exist := ehc.syncKeeper[msgID]
	return exist
}

func (ehc *EdgeHubController) sendToKeepChannel(message model.Message) error {
	ehc.keeperLock.RLock()
	defer ehc.keeperLock.RUnlock()

	channel, exist := ehc.syncKeeper[message.GetParentID()]
	if !exist {
		log.LOGGER.Errorf("failed to get sync keeper channel, messageID:%+v", message)
		return fmt.Errorf("failed to get sync keeper channel, messageID:%+v", message)
	}

	// send response into synckeep channel
	select {
	case channel <- message:
	default:
		log.LOGGER.Errorf("failed to send message to sync keep channel")
		return fmt.Errorf("failed to send message to sync keep channel")
	}

	return nil
}

func (ehc *EdgeHubController) dispatch(message model.Message) error {

	// TODO: dispatch message by the message type
	md, ok := groupMap[message.GetGroup()]
	if !ok {
		log.LOGGER.Warnf("msg_group not found")
		return fmt.Errorf("msg_group not found")
	}

	isResponse := ehc.isSyncResponse(message.GetParentID())
	if !isResponse {
		ehc.context.Send2Group(md, message)
		return nil
	}

	return ehc.sendToKeepChannel(message)
}

func (ehc *EdgeHubController) routeToEdge() {
	for {
		message, err := ehc.chClient.Receive()
		if err != nil {
			log.LOGGER.Errorf("websocket read error: %v", err)
			ehc.stopChan <- struct{}{}
			return
		}

		log.LOGGER.Infof("received msg from cloud-hub:%#v", message)
		err = ehc.dispatch(message)
		if err != nil {
			log.LOGGER.Errorf("failed to dispatch message, discard: %v", err)
		}
	}
}

func (ehc *EdgeHubController) sendToCloud(message model.Message) error {
	err := ehc.chClient.Send(message)
	if err != nil {
		log.LOGGER.Errorf("failed to send message: %v", err)
		return fmt.Errorf("failed to send message, error: %v", err)
	}

	syncKeep := func(message model.Message) {
		tempChannel := ehc.addKeepChannel(message.GetID())
		sendTimer := time.NewTimer(ehc.config.HeartbeatPeroid)
		select {
		case response := <-tempChannel:
			sendTimer.Stop()
			ehc.context.SendResp(response)
			ehc.deleteKeepChannel(response.GetParentID())
		case <-sendTimer.C:
			log.LOGGER.Warnf("timeout to receive response for message: %+v", message)
			ehc.deleteKeepChannel(message.GetID())
		}
	}

	if message.IsSync() {
		go syncKeep(message)
	}

	return nil
}

func (ehc *EdgeHubController) routeToCloud() {
	for {
		message, err := ehc.context.Receive(ModuleNameEdgeHub)
		if err != nil {
			log.LOGGER.Errorf("failed to receive message from edge: %v", err)
			time.Sleep(time.Second)
			continue
		}

		// post message to cloud hub
		err = ehc.sendToCloud(message)
		if err != nil {
			log.LOGGER.Errorf("failed to send message to cloud: %v", err)
			ehc.stopChan <- struct{}{}
			return
		}
	}
}

func (ehc *EdgeHubController) keepalive() {
	for {
		msg := model.NewMessage("").
			BuildRouter(ModuleNameEdgeHub, "resource", "node", "keepalive").
			FillBody("ping")
		err := ehc.chClient.Send(*msg)
		if err != nil {
			log.LOGGER.Errorf("websocket write error: %v", err)
			ehc.stopChan <- struct{}{}
			return
		}
		time.Sleep(ehc.config.HeartbeatPeroid)
	}
}

func (ehc *EdgeHubController) pubConnectInfo(isConnected bool) {
	// var info model.Message
	content := model.CLOUD_CONNECTED
	if !isConnected {
		content = model.CLOUD_DISCONNECTED
	}

	for _, group := range groupMap {
		message := model.NewMessage("").BuildRouter(message.SourceNodeConnection, group,
			message.ResourceTypeNodeConnection, message.OperationNodeConnection).FillBody(content)
		ehc.context.Send2Group(group, *message)
	}
}

func (ehc *EdgeHubController) postUrlRequst(client *http.Client) (string, error) {
	req, err := http_utils.BuildRequest(http.MethodGet, ehc.config.PlacementUrl, nil, "")
	if err != nil {
		log.LOGGER.Errorf("failed to build request: %v", err)
		return "", err
	}

	for {
		resp, err := http_utils.SendRequest(req, client)
		if err != nil {
			log.LOGGER.Errorf("%v", err)
			time.Sleep(time.Minute)
			continue
		}
		switch resp.StatusCode {
		case http.StatusOK:
			defer resp.Body.Close()
			bodyBytes, _ := ioutil.ReadAll(resp.Body)
			url := fmt.Sprintf("%s/%s/%s/events", string(bodyBytes), ehc.config.ProjectID, ehc.config.NodeId)
			log.LOGGER.Infof("successfully to get cloudaccess url: %s", url)
			return url, nil
		case http.StatusBadRequest:
			log.LOGGER.Errorf("no retry on error code: %d, failed to get cloudaccess url", resp.StatusCode)
			return "", fmt.Errorf("bad request")
		default:
			log.LOGGER.Errorf("get cloudaccess with Error code: %d", resp.StatusCode)
		}
		time.Sleep(time.Minute)
	}
}

func (ehc *EdgeHubController) getCloudHubUrl() (string, error) {
	// TODO: get the file path gracefully
	certFile := config.GetConfig().WSConfig.CertFilePath
	keyFile := config.GetConfig().WSConfig.KeyFilePath
	placementClient, err := http_utils.NewHTTPSclient(certFile, keyFile)
	if err != nil {
		log.LOGGER.Warnf("failed to new https client for placement, error: %+v", err)
		return "", fmt.Errorf("failed to new https client for placement, error: %+v", err)
	}

	cloudHubUrl, err := ehc.postUrlRequst(placementClient)
	if err != nil {
		log.LOGGER.Warnf("failed to get cloud hub url, error: %+v", err)
		return "", fmt.Errorf("failed to new https client for placement, error: %+v", err)
	}

	return cloudHubUrl, nil
}
