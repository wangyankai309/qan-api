/*
   Copyright (c) 2016, Percona LLC and/or its affiliates. All rights reserved.

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU Affero General Public License as published by
   the Free Software Foundation, either version 3 of the License, or
   (at your option) any later version.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Affero General Public License for more details.

   You should have received a copy of the GNU Affero General Public License
   along with this program.  If not, see <http://www.gnu.org/licenses/>
*/

package agent

import (
	"fmt"
	"io"
	"time"

	"github.com/cactus/go-statsd-client/statsd"
	"github.com/percona/qan-api/app/agent"
	"github.com/percona/qan-api/app/db"
	"github.com/percona/qan-api/app/models"
	"github.com/percona/qan-api/app/qan"
	"github.com/percona/qan-api/app/shared"
	"github.com/percona/qan-api/app/ws"
	"github.com/percona/qan-api/config"
	"github.com/percona/qan-api/stats"
	"github.com/revel/revel"
	"golang.org/x/net/websocket"
)

var msgStats stats.Stats

func init() {
	// The msgStats client must be copied and passed as a ref (see below).
	config.Get("statsd.server")
	statsClient, err := statsd.NewBufferedClient(config.Get("statsd.server"), "", 300*time.Millisecond, 8932)
	if err != nil {
		panic(fmt.Sprintf("statsd.NewBufferedClient: %s", err))
	}
	msgStats = stats.NewStats(
		statsClient,
		config.Get("stats.env"), // env (dev, test, stage, prod)
		config.Get("api.alias"), // server (api01, etc.)
		"data-in",
		config.Get("stats.rate"), // 0-1.0
	)
}

// Cmd -  WS /agents/:uuid/cmd
func (c Agent) Cmd(uuid string, conn *websocket.Conn) revel.Result {
	fmt.Println("AGENT CMD  ---- ")
	origin := c.Request.Header.Get("Origin")
	agentID := c.Args["agentId"].(uint)
	agentVersion := c.Args["agentVersion"].(string)
	prefix := fmt.Sprintf("[Agent.Cmd] agent_id=%d %s %s", agentID, agentVersion, origin)

	wsConn := ws.ExistingConnection(origin, c.Request.URL.String(), conn)
	defer wsConn.Disconnect()

	// When the agent disconnects, set oN.agent_configs.running=0 for the agent.
	agentConfigMgr := models.NewAgentConfigManager(c.Args["connsPool"])

	mx := ws.NewConcurrentMultiplexer(
		fmt.Sprintf("agent_id=%d", agentID),
		wsConn,
		agent.NewProcessor(agentID, agentConfigMgr),
		0, // 0 = serialize, no concurrency
	)

	fmt.Printf("ws.go:NewConcurrentMultiplexer: %+v \n", mx)

	// Create a local agent communicator and register it with the agent
	// direcotry so clients and other APIs can talk with this agent:
	//   agent <-[ws]-> this API(this controller(comm)) <-> clients
	// The communicator runs as long as the agent is connected and alive, or
	// until something stops it (which disconnects the agent).
	comm := agent.NewLocalAgent(agentID, mx)
	fmt.Printf("ws.go:NewLocalAgent: %+v \n", comm)
	err := comm.Start()
	if err != nil {
		fmt.Printf("ws.go:NewLocalAgent error: %+v \n", err)
		revel.WARN.Printf("%s Failed to start: %s", prefix, err)
		return nil
	}
	fmt.Println("ws.go:NewLocalAgent startrd!!!")
	defer comm.Stop()

	// Last step: register the agent with the local and global directories so
	// clients and other APIs can find and talk with it. Do this last to avoid
	// the race condition where agent is registered but its comm isn't ready.
	// Also, defer removing it from the dir last so this defer is ran first
	// (defer is LIFO) when the comm stops.
	fmt.Println("AGENT CMD  ---- shared.AgentDirectory.Add: ", agentID, comm)
	if err := shared.AgentDirectory.Add(agentID, comm); err != nil {
		revel.WARN.Printf("%s Failed to add to directory: %s", prefix, err)
		return nil
	}
	fmt.Println("AGENT CMD  --DONE-- shared.AgentDirectory.Add: ", agentID, comm)
	defer shared.AgentDirectory.Remove(agentID)

	revel.INFO.Printf("%s: connected", prefix)
	defer revel.INFO.Printf("%s: disconnected", prefix)

	<-comm.Done()
	return nil
}

// Data - WS /agents/:uuid/data
func (c Agent) Data(conn *websocket.Conn) revel.Result {
	origin := c.Request.Header.Get("Origin")
	agentID := c.Args["agentId"].(uint)

	fmt.Printf("=============\n Agent websocket Data: origin: %v, agentID: %v \n", origin, agentID)

	// Authenticate/authorize agent
	wsConn := ws.ExistingConnection(origin, c.Request.URL.String(), conn)
	defer wsConn.Disconnect()

	dbh := qan.NewMySQLMetricWriter(c.Args["connsPool"], shared.QueryAbstracter)

	// Read and queue log entries from agent.
	dataStats := msgStats // copy

	// Synchronous data transfer from agent to API: agent sends data as proto.Data,
	// API accepts, queues, and sends data.Response; repeat.
	if err := qan.SaveData(wsConn, agentID, dbh, &dataStats); err != nil {
		switch err {
		case io.EOF:
			// We got everything, client disconnected.
		default:
			return c.Error(err, "Agent.Data: qan.SaveData")
		}
	}

	return nil
}

func (c Agent) Log(conn *websocket.Conn) revel.Result {
	origin := c.Request.Header.Get("Origin")
	agentId := c.Args["agentId"].(uint)
	prefix := fmt.Sprintf("%s [Data.Log] agent_id=%d", origin, agentId)
	revel.TRACE.Println(prefix)

	wsConn := ws.ExistingConnection(origin, c.Request.URL.String(), conn)
	defer wsConn.Disconnect()

	dbStats := msgStats // copy
	dbm := db.NewMySQLManager()
	if err := dbm.Open(); err != nil {
		return c.Error(err, "Agent.Log: dbm.Open")
	}
	defer dbm.Close()
	dbh := agent.NewLogHandler(dbm, &dbStats)

	// todo: use multiplexer
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	// Read and queue log entries from agent.
	logStats := msgStats // copy
	if err := agent.SaveLog(wsConn, agentId, ticker.C, dbh, &logStats); err != nil {
		switch err {
		case io.EOF:
			revel.TRACE.Printf("%s: done (EOF)", prefix)
		default:
			return c.Error(err, "Agent.Log: agent.SaveLog")
		}
	}

	return nil
}
