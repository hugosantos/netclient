package functions

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/devilcove/httpclient"
	"github.com/gravitl/netclient/config"
	"github.com/gravitl/netclient/daemon"
	"github.com/gravitl/netclient/wireguard"
	"github.com/gravitl/netmaker/logger"
	"github.com/gravitl/netmaker/models"
)

// Pull - pulls the latest config from the server, if manual it will overwrite
func Pull(network string, iface bool) (*config.Node, error) {
	node := config.GetNode(network)
	if node.Network == "" {
		return nil, errors.New("no such network")
	}
	server := config.GetServer(node.Server)
	token, err := Authenticate(&node, config.Netclient())
	if err != nil {
		return nil, err
	}
	endpoint := httpclient.JSONEndpoint[models.NodeGet, models.ErrorResponse]{
		URL:           "https://" + server.API,
		Route:         "/api/nodes/" + node.Network + "/" + node.ID,
		Method:        http.MethodGet,
		Authorization: "Bearer " + token,
		Response:      models.NodeGet{},
		ErrorResponse: models.ErrorResponse{},
	}
	response, errData, err := endpoint.GetJSON(models.NodeGet{}, models.ErrorResponse{})
	if err != nil {
		if errors.Is(err, httpclient.ErrStatus) {
			logger.Log(0, "errror getting node", strconv.Itoa(errData.Code), errData.Message)
		}
		return nil, err
	}
	nodeGet := response
	newNode, newServer, newHost := config.ConvertNode(&nodeGet)
	config.UpdateNodeMap(newNode.Network, *newNode)
	if err = config.WriteNodeConfig(); err != nil {
		return nil, err
	}
	config.SaveServer(newNode.Server, *newServer)
	config.WriteNodeConfig()
	config.WriteNetclientConfig()
	//update wg config
	peers := newNode.Peers
	for _, node := range config.GetNodes() {
		if node.Connected {
			peers = append(peers, node.Peers...)
		}
	}
	internetGateway, err := wireguard.UpdateWgPeers(peers)
	if internetGateway != nil && err != nil {
		newHost.InternetGateway = *internetGateway
		config.WriteNetclientConfig()
	}
	logger.Log(1, "node settings for network ", network)
	if config.Netclient().DaemonInstalled {
		logger.Log(3, "restarting daemon")
		if err := daemon.Restart(); err != nil {
			if err := daemon.Start(); err != nil {
				return newNode, err
			}
		}
	}
	return newNode, err
}
