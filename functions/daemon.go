package functions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/gravitl/netclient/config"
	"github.com/gravitl/netclient/daemon"
	"github.com/gravitl/netclient/local"
	"github.com/gravitl/netclient/ncutils"
	"github.com/gravitl/netclient/networking"
	"github.com/gravitl/netclient/nmproxy"
	proxy_cfg "github.com/gravitl/netclient/nmproxy/config"
	ncmodels "github.com/gravitl/netclient/nmproxy/models"
	"github.com/gravitl/netclient/nmproxy/stun"
	"github.com/gravitl/netclient/routes"
	"github.com/gravitl/netclient/wireguard"
	"github.com/gravitl/netmaker/logger"
	"github.com/gravitl/netmaker/models"
	"github.com/gravitl/netmaker/mq"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

const (
	lastNodeUpdate   = "lnu"
	lastDNSUpdate    = "ldu"
	lastALLDNSUpdate = "ladu"
)

var (
	messageCache     = new(sync.Map)
	ServerSet        = make(map[string]mqtt.Client)
	ProxyManagerChan = make(chan *models.HostPeerUpdate, 50)
	hostNatInfo      *ncmodels.HostInfo
)

type cachedMessage struct {
	Message  string
	LastSeen time.Time
}

func startProxy(wg *sync.WaitGroup) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	wg.Add(1)
	go nmproxy.Start(ctx, wg, ProxyManagerChan, hostNatInfo, config.Netclient().ProxyListenPort)
	return cancel
}

// Daemon runs netclient daemon
func Daemon() {
	logger.Log(0, "netclient daemon started -- version:", config.Version)
	if err := ncutils.SavePID(); err != nil {
		logger.FatalLog("unable to save PID on daemon startup")
	}
	if err := local.SetIPForwarding(); err != nil {
		logger.Log(0, "unable to set IPForwarding", err.Error())
	}
	wg := sync.WaitGroup{}
	quit := make(chan os.Signal, 1)
	reset := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, os.Interrupt)
	signal.Notify(reset, syscall.SIGHUP)

	shouldUpdateNat := getNatInfo()
	if shouldUpdateNat { // will be reported on check-in
		if err := config.WriteNetclientConfig(); err == nil {
			logger.Log(1, "updated NAT type to", hostNatInfo.NatType)
		}
	}
	cancel := startGoRoutines(&wg)
	stopProxy := startProxy(&wg)
	//start httpserver on its own -- doesn't need to restart on reset
	httpctx, httpCancel := context.WithCancel(context.Background())
	httpWg := sync.WaitGroup{}
	httpWg.Add(1)
	go HttpServer(httpctx, &httpWg)
	for {
		select {
		case <-quit:
			logger.Log(0, "shutting down netclient daemon")
			closeRoutines([]context.CancelFunc{
				cancel,
				stopProxy,
			}, &wg)
			httpCancel()
			httpWg.Wait()
			logger.Log(0, "shutdown complete")
			return
		case <-reset:
			logger.Log(0, "received reset")
			closeRoutines([]context.CancelFunc{
				cancel,
				stopProxy,
			}, &wg)
			logger.Log(0, "restarting daemon")
			shouldUpdateNat := getNatInfo()
			if shouldUpdateNat { // will be reported on check-in
				if err := config.WriteNetclientConfig(); err == nil {
					logger.Log(1, "updated NAT type to", hostNatInfo.NatType)
				}
			}
			cleanUpRoutes()
			cancel = startGoRoutines(&wg)
			if !proxy_cfg.GetCfg().ProxyStatus {
				stopProxy = startProxy(&wg)
			}
		}
	}
}

func closeRoutines(closers []context.CancelFunc, wg *sync.WaitGroup) {
	for i := range closers {
		closers[i]()
	}
	for _, mqclient := range ServerSet {
		if mqclient != nil {
			mqclient.Disconnect(250)
		}
	}
	wg.Wait()
	logger.Log(0, "closing netmaker interface")
	iface := wireguard.GetInterface()
	iface.Close()
}

// startGoRoutines starts the daemon goroutines
func startGoRoutines(wg *sync.WaitGroup) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := config.ReadNetclientConfig(); err != nil {
		logger.Log(0, "error reading neclient config file", err.Error())
	}
	config.UpdateNetclient(*config.Netclient())
	if err := config.ReadNodeConfig(); err != nil {
		logger.Log(0, "error reading node map from disk", err.Error())
	}
	if err := config.ReadServerConf(); err != nil {
		logger.Log(0, "errors reading server map from disk", err.Error())
	}
	logger.Log(3, "configuring netmaker wireguard interface")
	nc := wireguard.NewNCIface(config.Netclient(), config.GetNodes())
	nc.Create()
	nc.Configure()
	if len(config.Servers) == 0 {
		ProxyManagerChan <- &models.HostPeerUpdate{
			ProxyUpdate: models.ProxyManagerPayload{
				Action: models.ProxyDeleteAllPeers,
			},
		}
	}
	for _, server := range config.Servers {
		logger.Log(1, "started daemon for server ", server.Name)
		server := server
		networking.StoreServerAddresses(&server)
		err := routes.SetNetmakerServerRoutes(config.Netclient().DefaultInterface, &server)
		if err != nil {
			logger.Log(2, "failed to set route(s) for", server.Name, err.Error())
		}
		wg.Add(1)
		go messageQueue(ctx, wg, &server)
	}
	wireguard.SetPeers()
	if err := routes.SetNetmakerPeerEndpointRoutes(config.Netclient().DefaultInterface); err != nil {
		logger.Log(2, "failed to set initial peer routes", err.Error())
	}
	wg.Add(1)
	go Checkin(ctx, wg)
	wg.Add(1)
	go networking.StartIfaceDetection(ctx, wg, config.Netclient().ProxyListenPort)
	return cancel
}

// sets up Message Queue and subsribes/publishes updates to/from server
// the client should subscribe to ALL nodes that exist on server locally
func messageQueue(ctx context.Context, wg *sync.WaitGroup, server *config.Server) {
	defer wg.Done()
	logger.Log(0, "netclient message queue started for server:", server.Name)
	err := setupMQTT(server)
	if err != nil {
		logger.Log(0, "unable to connect to broker", server.Broker, err.Error())
		return
	}
	defer ServerSet[server.Name].Disconnect(250)
	<-ctx.Done()
	logger.Log(0, "shutting down message queue for server", server.Name)
}

// setupMQTT creates a connection to broker
func setupMQTT(server *config.Server) error {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(server.Broker)
	opts.SetUsername(server.MQUserName)
	opts.SetPassword(server.MQPassword)
	//opts.SetClientID(ncutils.MakeRandomString(23))
	opts.SetClientID(server.MQID.String())
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(time.Second << 2)
	opts.SetKeepAlive(time.Second * 10)
	opts.SetWriteTimeout(time.Minute)
	opts.SetOnConnectHandler(func(client mqtt.Client) {
		logger.Log(0, "mqtt connect handler")
		nodes := config.GetNodes()
		for _, node := range nodes {
			node := node
			setSubscriptions(client, &node)
		}
		setHostSubscription(client, server.Name)
	})
	opts.SetOrderMatters(true)
	opts.SetResumeSubs(true)
	opts.SetConnectionLostHandler(func(c mqtt.Client, e error) {
		logger.Log(0, "detected broker connection lost for", server.Broker)
		if ok := resetServerRoutes(); ok {
			logger.Log(0, "detected default gw change, reset routes")
			if err := UpdateHostSettings(); err != nil {
				logger.Log(0, "failed to update host settings -", err.Error())
				return
			}

			handlePeerInetGateways(
				!config.GW4PeerDetected && !config.GW6PeerDetected,
				config.IsHostInetGateway(), false,
				nil,
			)
		}
	})
	mqclient := mqtt.NewClient(opts)
	ServerSet[server.Name] = mqclient
	var connecterr error
	for count := 0; count < 3; count++ {
		connecterr = nil
		if token := mqclient.Connect(); !token.WaitTimeout(30*time.Second) || token.Error() != nil {
			logger.Log(0, "unable to connect to broker, retrying ...")
			if token.Error() == nil {
				connecterr = errors.New("connect timeout")
			} else {
				connecterr = token.Error()
			}
		}
	}
	if connecterr != nil {
		logger.Log(0, "failed to establish connection to broker: ", connecterr.Error())
		return connecterr
	}
	if err := PublishHostUpdate(server.Name, models.Acknowledgement); err != nil {
		logger.Log(0, "failed to send initial ACK to server", server.Name, err.Error())
	} else {
		logger.Log(2, "successfully requested ACK on server", server.Name)
	}
	// send register signal with turn to server
	if server.UseTurn {
		if err := PublishHostUpdate(server.Server, models.RegisterWithTurn); err != nil {
			logger.Log(0, "failed to publish host turn register signal to server:", server.Server, err.Error())
		} else {
			logger.Log(0, "published host turn register signal to server:", server.Server)
		}
	}

	return nil
}

// func setMQTTSingenton creates a connection to broker for single use (ie to publish a message)
// only to be called from cli (eg. connect/disconnect, join, leave) and not from daemon ---
func setupMQTTSingleton(server *config.Server, publishOnly bool) error {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(server.Broker)
	opts.SetUsername(server.MQUserName)
	opts.SetPassword(server.MQPassword)
	opts.SetClientID(server.MQID.String())
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(time.Second << 2)
	opts.SetKeepAlive(time.Minute >> 1)
	opts.SetWriteTimeout(time.Minute)
	opts.SetOnConnectHandler(func(client mqtt.Client) {
		if !publishOnly {
			logger.Log(0, "mqtt connect handler")
			nodes := config.GetNodes()
			for _, node := range nodes {
				node := node
				setSubscriptions(client, &node)
			}
			setHostSubscription(client, server.Name)
		}
		logger.Log(1, "successfully connected to", server.Broker)
	})
	opts.SetOrderMatters(true)
	opts.SetResumeSubs(true)
	opts.SetConnectionLostHandler(func(c mqtt.Client, e error) {
		logger.Log(0, "detected broker connection lost for", server.Broker)
	})
	mqclient := mqtt.NewClient(opts)
	ServerSet[server.Name] = mqclient
	var connecterr error
	if token := mqclient.Connect(); !token.WaitTimeout(30*time.Second) || token.Error() != nil {
		logger.Log(0, "unable to connect to broker,", server.Broker+",", "retrying...")
		if token.Error() == nil {
			connecterr = errors.New("connect timeout")
		} else {
			connecterr = token.Error()
		}
	}
	return connecterr
}

// setHostSubscription sets MQ client subscriptions for host
// should be called for each server host is registered on.
func setHostSubscription(client mqtt.Client, server string) {
	hostID := config.Netclient().ID
	logger.Log(3, fmt.Sprintf("subscribed to host peer updates  peers/host/%s/%s", hostID.String(), server))
	if token := client.Subscribe(fmt.Sprintf("peers/host/%s/%s", hostID.String(), server), 0, mqtt.MessageHandler(HostPeerUpdate)); token.Wait() && token.Error() != nil {
		logger.Log(0, "MQ host sub: ", hostID.String(), token.Error().Error())
		return
	}
	logger.Log(3, fmt.Sprintf("subscribed to host updates  host/update/%s/%s", hostID.String(), server))
	if token := client.Subscribe(fmt.Sprintf("host/update/%s/%s", hostID.String(), server), 0, mqtt.MessageHandler(HostUpdate)); token.Wait() && token.Error() != nil {
		logger.Log(0, "MQ host sub: ", hostID.String(), token.Error().Error())
		return
	}
	logger.Log(3, fmt.Sprintf("subcribed to dns updates dns/update/%s/%s", hostID.String(), server))
	if token := client.Subscribe(fmt.Sprintf("dns/update/%s/%s", hostID.String(), server), 0, mqtt.MessageHandler(dnsUpdate)); token.Wait() && token.Error() != nil {
		logger.Log(0, "MQ host sub: ", hostID.String(), token.Error().Error())
		return
	}
	logger.Log(3, fmt.Sprintf("subcribed to all dns updates dns/all/%s/%s", hostID.String(), server))
	if token := client.Subscribe(fmt.Sprintf("dns/all/%s/%s", hostID.String(), server), 0, mqtt.MessageHandler(dnsAll)); token.Wait() && token.Error() != nil {
		logger.Log(0, "MQ host sub: ", hostID.String(), token.Error().Error())
		return
	}
}

// setSubcriptions sets MQ client subscriptions for a specific node config
// should be called for each node belonging to a given server
func setSubscriptions(client mqtt.Client, node *config.Node) {
	if token := client.Subscribe(fmt.Sprintf("node/update/%s/%s", node.Network, node.ID), 0, mqtt.MessageHandler(NodeUpdate)); token.WaitTimeout(mq.MQ_TIMEOUT*time.Second) && token.Error() != nil {
		if token.Error() == nil {
			logger.Log(0, "network:", node.Network, "connection timeout")
		} else {
			logger.Log(0, "network:", node.Network, token.Error().Error())
		}
		return
	}
	logger.Log(3, fmt.Sprintf("subscribed to peer updates peers/%s/%s", node.Network, node.ID))
}

// should only ever use node client configs
func decryptMsg(serverName string, msg []byte) ([]byte, error) {
	if len(msg) <= 24 { // make sure message is of appropriate length
		return nil, fmt.Errorf("received invalid message from broker %v", msg)
	}
	host := config.Netclient()
	// setup the keys
	diskKey, err := ncutils.ConvertBytesToKey(host.TrafficKeyPrivate)
	if err != nil {
		return nil, err
	}

	server := config.GetServer(serverName)
	if server == nil {
		return nil, errors.New("nil server for " + serverName)
	}
	serverPubKey, err := ncutils.ConvertBytesToKey(server.TrafficKey)
	if err != nil {
		return nil, err
	}
	return DeChunk(msg, serverPubKey, diskKey)
}

func read(network, which string) string {
	val, isok := messageCache.Load(fmt.Sprintf("%s%s", network, which))
	if isok {
		var readMessage = val.(cachedMessage) // fetch current cached message
		if readMessage.LastSeen.IsZero() {
			return ""
		}
		if time.Now().After(readMessage.LastSeen.Add(time.Hour * 24)) { // check if message has been there over a minute
			messageCache.Delete(fmt.Sprintf("%s%s", network, which)) // remove old message if expired
			return ""
		}
		return readMessage.Message // return current message if not expired
	}
	return ""
}

func insert(network, which, cache string) {
	var newMessage = cachedMessage{
		Message:  cache,
		LastSeen: time.Now(),
	}
	messageCache.Store(fmt.Sprintf("%s%s", network, which), newMessage)
}

// on a delete usually, pass in the nodecfg to unsubscribe client broker communications
// for the node in nodeCfg
func unsubscribeNode(client mqtt.Client, node *config.Node) {
	var ok = true
	if token := client.Unsubscribe(fmt.Sprintf("node/update/%s/%s", node.Network, node.ID)); token.WaitTimeout(mq.MQ_TIMEOUT*time.Second) && token.Error() != nil {
		if token.Error() == nil {
			logger.Log(1, "network:", node.Network, "unable to unsubscribe from updates for node ", node.ID.String(), "\n", "connection timeout")
		} else {
			logger.Log(1, "network:", node.Network, "unable to unsubscribe from updates for node ", node.ID.String(), "\n", token.Error().Error())
		}
		ok = false
	} // peer updates belong to host now

	if ok {
		logger.Log(1, "network:", node.Network, "successfully unsubscribed node ", node.ID.String())
	}
}

// unsubscribe client broker communications for host topics
func unsubscribeHost(client mqtt.Client, server string) {
	hostID := config.Netclient().ID
	logger.Log(3, fmt.Sprintf("removing subscription for host peer updates peers/host/%s/%s", hostID.String(), server))
	if token := client.Unsubscribe(fmt.Sprintf("peers/host/%s/%s", hostID.String(), server)); token.WaitTimeout(mq.MQ_TIMEOUT*time.Second) && token.Error() != nil {
		logger.Log(0, "unable to unsubscribe from host peer updates: ", hostID.String(), token.Error().Error())
		return
	}
	logger.Log(3, fmt.Sprintf("removing subscription for host updates  host/update/%s/%s", hostID.String(), server))
	if token := client.Unsubscribe(fmt.Sprintf("host/update/%s/%s", hostID.String(), server)); token.WaitTimeout(mq.MQ_TIMEOUT*time.Second) && token.Error() != nil {
		logger.Log(0, "unable to unsubscribe from host updates: ", hostID.String(), token.Error().Error())
		return
	}
}

// UpdateKeys -- updates private key and returns new publickey
func UpdateKeys() error {
	var err error
	logger.Log(0, "received message to update wireguard keys ")
	host := config.Netclient()
	host.PrivateKey, err = wgtypes.GeneratePrivateKey()
	if err != nil {
		logger.Log(0, "error generating privatekey ", err.Error())
		return err
	}
	file := config.GetNetclientPath() + "netmaker.conf"
	if err := wireguard.UpdatePrivateKey(file, host.PrivateKey.String()); err != nil {
		logger.Log(0, "error updating wireguard key ", err.Error())
		return err
	}
	host.PublicKey = host.PrivateKey.PublicKey()
	if err := config.WriteNetclientConfig(); err != nil {
		logger.Log(0, "error saving netclient config", err.Error())
	}
	PublishGlobalHostUpdate(models.UpdateHost)
	daemon.Restart()
	return nil
}

// RemoveServer - removes a server from server conf given a specific node
func RemoveServer(node *config.Node) {
	logger.Log(0, "removing server", node.Server, "from mq")
	delete(ServerSet, node.Server)
}

func getNatInfo() (natUpdated bool) {
	ncConf, err := config.ReadNetclientConfig()
	if err != nil {
		logger.Log(0, "errors reading netclient from disk", err.Error())
		return
	}
	err = config.ReadServerConf()
	if err != nil {
		logger.Log(0, "errors reading server map from disk", err.Error())
		return
	}

	for _, server := range config.Servers {
		server := server
		if hostNatInfo == nil {
			portToStun, err := ncutils.GetFreePort(config.Netclient().ProxyListenPort)
			if portToStun == 0 || err != nil {
				portToStun = config.Netclient().ListenPort
			}

			hostNatInfo = stun.GetHostNatInfo(
				server.StunList,
				config.Netclient().EndpointIP.String(),
				portToStun,
			)
			if len(ncConf.Host.NatType) == 0 || ncConf.Host.NatType != hostNatInfo.NatType {
				config.Netclient().Host.NatType = hostNatInfo.NatType
				return true
			}
		}
	}
	return
}

func cleanUpRoutes() {
	gwAddr := config.GW4Addr
	if gwAddr.IP == nil {
		gwAddr = config.GW6Addr
	}
	if err := routes.CleanUp(config.Netclient().DefaultInterface, &gwAddr); err != nil {
		logger.Log(0, "routes not completely cleaned up", err.Error())
	}
}

func resetServerRoutes() bool {
	if routes.HasGatewayChanged() {
		cleanUpRoutes()
		for _, server := range config.Servers {
			server := server
			if err := routes.SetNetmakerServerRoutes(config.Netclient().DefaultInterface, &server); err != nil {
				logger.Log(2, "failed to set route(s) for", server.Name, err.Error())
			}
			if err := routes.SetNetmakerPeerEndpointRoutes(config.Netclient().DefaultInterface); err != nil {
				logger.Log(2, "failed to set route(s) for", server.Name, err.Error())
			}
		}
		return true
	}
	return false
}
