package nomad

import (
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/client"
	"github.com/hashicorp/nomad/client/config"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/stretchr/testify/require"
	codec "github.com/ugorji/go/codec"
)

func TestClientFS_Logs_NoAlloc(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	// Start a server and client
	s := TestServer(t, nil)
	defer s.Shutdown()
	testutil.WaitForLeader(t, s.RPC)

	// Make the request with bad allocation id
	req := &cstructs.FsLogsRequest{
		AllocID:      uuid.Generate(),
		QueryOptions: structs.QueryOptions{Region: "global"},
	}

	// Get the handler
	handler, err := s.StreamingRpcHandler("FileSystem.Logs")
	require.Nil(err)

	// Create a pipe
	p1, p2 := net.Pipe()
	defer p1.Close()
	defer p2.Close()

	errCh := make(chan error)
	streamMsg := make(chan *cstructs.StreamErrWrapper)

	// Start the handler
	go handler(p2)

	// Start the decoder
	go func() {
		decoder := codec.NewDecoder(p1, structs.MsgpackHandle)
		for {
			var msg cstructs.StreamErrWrapper
			if err := decoder.Decode(&msg); err != nil {
				if err == io.EOF || strings.Contains(err.Error(), "closed") {
					return
				}
				errCh <- fmt.Errorf("error decoding: %v", err)
			}

			streamMsg <- &msg
		}
	}()

	// Send the request
	encoder := codec.NewEncoder(p1, structs.MsgpackHandle)
	require.Nil(encoder.Encode(req))

	timeout := time.After(5 * time.Second)

OUTER:
	for {
		select {
		case <-timeout:
			t.Fatal("timeout")
		case err := <-errCh:
			t.Fatal(err)
		case msg := <-streamMsg:
			if msg.Error == nil {
				continue
			}

			if strings.Contains(msg.Error.Error(), "unknown alloc") {
				break OUTER
			}
		}
	}
}

func TestClientFS_Logs_ACL(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	// Start a server
	s, root := TestACLServer(t, nil)
	defer s.Shutdown()
	testutil.WaitForLeader(t, s.RPC)

	// Create a bad token
	policyBad := mock.NamespacePolicy("other", "", []string{acl.NamespaceCapabilityReadFS})
	tokenBad := mock.CreatePolicyAndToken(t, s.State(), 1005, "invalid", policyBad)

	policyGood := mock.NamespacePolicy(structs.DefaultNamespace, "",
		[]string{acl.NamespaceCapabilityReadLogs, acl.NamespaceCapabilityReadFS})
	tokenGood := mock.CreatePolicyAndToken(t, s.State(), 1009, "valid2", policyGood)

	cases := []struct {
		Name          string
		Token         string
		ExpectedError string
	}{
		{
			Name:          "bad token",
			Token:         tokenBad.SecretID,
			ExpectedError: structs.ErrPermissionDenied.Error(),
		},
		{
			Name:          "good token",
			Token:         tokenGood.SecretID,
			ExpectedError: "unknown alloc ID",
		},
		{
			Name:          "root token",
			Token:         root.SecretID,
			ExpectedError: "unknown alloc ID",
		},
	}

	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			// Make the request with bad allocation id
			req := &cstructs.FsLogsRequest{
				AllocID: uuid.Generate(),
				QueryOptions: structs.QueryOptions{
					Namespace: structs.DefaultNamespace,
					Region:    "global",
					AuthToken: c.Token,
				},
			}

			// Get the handler
			handler, err := s.StreamingRpcHandler("FileSystem.Logs")
			require.Nil(err)

			// Create a pipe
			p1, p2 := net.Pipe()
			defer p1.Close()
			defer p2.Close()

			errCh := make(chan error)
			streamMsg := make(chan *cstructs.StreamErrWrapper)

			// Start the handler
			go handler(p2)

			// Start the decoder
			go func() {
				decoder := codec.NewDecoder(p1, structs.MsgpackHandle)
				for {
					var msg cstructs.StreamErrWrapper
					if err := decoder.Decode(&msg); err != nil {
						if err == io.EOF || strings.Contains(err.Error(), "closed") {
							return
						}
						errCh <- fmt.Errorf("error decoding: %v", err)
					}

					streamMsg <- &msg
				}
			}()

			// Send the request
			encoder := codec.NewEncoder(p1, structs.MsgpackHandle)
			require.Nil(encoder.Encode(req))

			timeout := time.After(5 * time.Second)

		OUTER:
			for {
				select {
				case <-timeout:
					t.Fatal("timeout")
				case err := <-errCh:
					t.Fatal(err)
				case msg := <-streamMsg:
					if msg.Error == nil {
						continue
					}

					if strings.Contains(msg.Error.Error(), c.ExpectedError) {
						break OUTER
					} else {
						t.Fatalf("Bad error: %v", msg.Error)
					}
				}
			}
		})
	}
}

func TestClientFS_Logs_Local(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	// Start a server and client
	s := TestServer(t, nil)
	defer s.Shutdown()
	testutil.WaitForLeader(t, s.RPC)

	c := client.TestClient(t, func(c *config.Config) {
		c.Servers = []string{s.config.RPCAddr.String()}
	})
	defer c.Shutdown()

	// Force an allocation onto the node
	expected := "Hello from the other side"
	a := mock.Alloc()
	a.Job.Type = structs.JobTypeBatch
	a.NodeID = c.NodeID()
	a.Job.TaskGroups[0].Count = 1
	a.Job.TaskGroups[0].Tasks[0] = &structs.Task{
		Name:   "web",
		Driver: "mock_driver",
		Config: map[string]interface{}{
			"run_for":       "2s",
			"stdout_string": expected,
		},
		LogConfig: structs.DefaultLogConfig(),
		Resources: &structs.Resources{
			CPU:      500,
			MemoryMB: 256,
		},
	}

	// Wait for the client to connect
	testutil.WaitForResult(func() (bool, error) {
		nodes := s.connectedNodes()
		return len(nodes) == 1, nil
	}, func(err error) {
		t.Fatalf("should have a clients")
	})

	// Upsert the allocation
	state := s.State()
	require.Nil(state.UpsertJob(999, a.Job))
	require.Nil(state.UpsertAllocs(1003, []*structs.Allocation{a}))

	// Wait for the client to run the allocation
	testutil.WaitForResult(func() (bool, error) {
		alloc, err := state.AllocByID(nil, a.ID)
		if err != nil {
			return false, err
		}
		if alloc == nil {
			return false, fmt.Errorf("unknown alloc")
		}
		if alloc.ClientStatus != structs.AllocClientStatusComplete {
			return false, fmt.Errorf("alloc client status: %v", alloc.ClientStatus)
		}

		return true, nil
	}, func(err error) {
		t.Fatalf("Alloc on node %q not finished: %v", c.NodeID(), err)
	})

	// Make the request
	req := &cstructs.FsLogsRequest{
		AllocID:      a.ID,
		Task:         a.Job.TaskGroups[0].Tasks[0].Name,
		LogType:      "stdout",
		Origin:       "start",
		PlainText:    true,
		QueryOptions: structs.QueryOptions{Region: "global"},
	}

	// Get the handler
	handler, err := s.StreamingRpcHandler("FileSystem.Logs")
	require.Nil(err)

	// Create a pipe
	p1, p2 := net.Pipe()
	defer p1.Close()
	defer p2.Close()

	errCh := make(chan error)
	streamMsg := make(chan *cstructs.StreamErrWrapper)

	// Start the handler
	go handler(p2)

	// Start the decoder
	go func() {
		decoder := codec.NewDecoder(p1, structs.MsgpackHandle)
		for {
			var msg cstructs.StreamErrWrapper
			if err := decoder.Decode(&msg); err != nil {
				if err == io.EOF || strings.Contains(err.Error(), "closed") {
					return
				}
				errCh <- fmt.Errorf("error decoding: %v", err)
			}

			streamMsg <- &msg
		}
	}()

	// Send the request
	encoder := codec.NewEncoder(p1, structs.MsgpackHandle)
	require.Nil(encoder.Encode(req))

	timeout := time.After(3 * time.Second)
	received := ""
OUTER:
	for {
		select {
		case <-timeout:
			t.Fatal("timeout")
		case err := <-errCh:
			t.Fatal(err)
		case msg := <-streamMsg:
			if msg.Error != nil {
				t.Fatalf("Got error: %v", msg.Error.Error())
			}

			// Add the payload
			received += string(msg.Payload)
			if received == expected {
				break OUTER
			}
		}
	}
}

func TestClientFS_Logs_Local_Follow(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	// Start a server and client
	s := TestServer(t, nil)
	defer s.Shutdown()
	testutil.WaitForLeader(t, s.RPC)

	c := client.TestClient(t, func(c *config.Config) {
		c.Servers = []string{s.config.RPCAddr.String()}
	})
	defer c.Shutdown()

	// Force an allocation onto the node
	expectedBase := "Hello from the other side"
	repeat := 10

	a := mock.Alloc()
	a.Job.Type = structs.JobTypeBatch
	a.NodeID = c.NodeID()
	a.Job.TaskGroups[0].Count = 1
	a.Job.TaskGroups[0].Tasks[0] = &structs.Task{
		Name:   "web",
		Driver: "mock_driver",
		Config: map[string]interface{}{
			"run_for":                "20s",
			"stdout_string":          expectedBase,
			"stdout_repeat":          repeat,
			"stdout_repeat_duration": 200 * time.Millisecond,
		},
		LogConfig: structs.DefaultLogConfig(),
		Resources: &structs.Resources{
			CPU:      500,
			MemoryMB: 256,
		},
	}

	// Wait for the client to connect
	testutil.WaitForResult(func() (bool, error) {
		nodes := s.connectedNodes()
		return len(nodes) == 1, nil
	}, func(err error) {
		t.Fatalf("should have a clients")
	})

	// Upsert the allocation
	state := s.State()
	require.Nil(state.UpsertJob(999, a.Job))
	require.Nil(state.UpsertAllocs(1003, []*structs.Allocation{a}))

	// Wait for the client to run the allocation
	testutil.WaitForResult(func() (bool, error) {
		alloc, err := state.AllocByID(nil, a.ID)
		if err != nil {
			return false, err
		}
		if alloc == nil {
			return false, fmt.Errorf("unknown alloc")
		}
		if alloc.ClientStatus != structs.AllocClientStatusRunning {
			return false, fmt.Errorf("alloc client status: %v", alloc.ClientStatus)
		}

		return true, nil
	}, func(err error) {
		t.Fatalf("Alloc on node %q not running: %v", c.NodeID(), err)
	})

	// Make the request
	req := &cstructs.FsLogsRequest{
		AllocID:      a.ID,
		Task:         a.Job.TaskGroups[0].Tasks[0].Name,
		LogType:      "stdout",
		Origin:       "start",
		PlainText:    true,
		Follow:       true,
		QueryOptions: structs.QueryOptions{Region: "global"},
	}

	// Get the handler
	handler, err := s.StreamingRpcHandler("FileSystem.Logs")
	require.Nil(err)

	// Create a pipe
	p1, p2 := net.Pipe()
	defer p1.Close()
	defer p2.Close()

	errCh := make(chan error)
	streamMsg := make(chan *cstructs.StreamErrWrapper)

	// Start the handler
	go handler(p2)

	// Start the decoder
	go func() {
		decoder := codec.NewDecoder(p1, structs.MsgpackHandle)
		for {
			var msg cstructs.StreamErrWrapper
			if err := decoder.Decode(&msg); err != nil {
				if err == io.EOF || strings.Contains(err.Error(), "closed") {
					return
				}
				errCh <- fmt.Errorf("error decoding: %v", err)
			}

			streamMsg <- &msg
		}
	}()

	// Send the request
	encoder := codec.NewEncoder(p1, structs.MsgpackHandle)
	require.Nil(encoder.Encode(req))

	timeout := time.After(20 * time.Second)
	expected := strings.Repeat(expectedBase, repeat+1)
	received := ""
OUTER:
	for {
		select {
		case <-timeout:
			t.Fatal("timeout")
		case err := <-errCh:
			t.Fatal(err)
		case msg := <-streamMsg:
			if msg.Error != nil {
				t.Fatalf("Got error: %v", msg.Error.Error())
			}

			// Add the payload
			received += string(msg.Payload)
			if received == expected {
				break OUTER
			}
		}
	}
}

func TestClientFS_Logs_Remote_Server(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	// Start a server and client
	s1 := TestServer(t, nil)
	defer s1.Shutdown()
	s2 := TestServer(t, func(c *Config) {
		c.DevDisableBootstrap = true
	})
	defer s2.Shutdown()
	TestJoin(t, s1, s2)
	testutil.WaitForLeader(t, s1.RPC)
	testutil.WaitForLeader(t, s2.RPC)

	c := client.TestClient(t, func(c *config.Config) {
		c.Servers = []string{s2.config.RPCAddr.String()}
	})
	defer c.Shutdown()

	// Force an allocation onto the node
	expected := "Hello from the other side"
	a := mock.Alloc()
	a.Job.Type = structs.JobTypeBatch
	a.NodeID = c.NodeID()
	a.Job.TaskGroups[0].Count = 1
	a.Job.TaskGroups[0].Tasks[0] = &structs.Task{
		Name:   "web",
		Driver: "mock_driver",
		Config: map[string]interface{}{
			"run_for":       "2s",
			"stdout_string": expected,
		},
		LogConfig: structs.DefaultLogConfig(),
		Resources: &structs.Resources{
			CPU:      500,
			MemoryMB: 256,
		},
	}

	// Wait for the client to connect
	testutil.WaitForResult(func() (bool, error) {
		nodes := s2.connectedNodes()
		return len(nodes) == 1, nil
	}, func(err error) {
		t.Fatalf("should have a clients")
	})

	// Upsert the allocation
	state1 := s1.State()
	state2 := s2.State()
	require.Nil(state1.UpsertJob(999, a.Job))
	require.Nil(state1.UpsertAllocs(1003, []*structs.Allocation{a}))
	require.Nil(state2.UpsertJob(999, a.Job))
	require.Nil(state2.UpsertAllocs(1003, []*structs.Allocation{a}))

	// Wait for the client to run the allocation
	testutil.WaitForResult(func() (bool, error) {
		alloc, err := state2.AllocByID(nil, a.ID)
		if err != nil {
			return false, err
		}
		if alloc == nil {
			return false, fmt.Errorf("unknown alloc")
		}
		if alloc.ClientStatus != structs.AllocClientStatusComplete {
			return false, fmt.Errorf("alloc client status: %v", alloc.ClientStatus)
		}

		return true, nil
	}, func(err error) {
		t.Fatalf("Alloc on node %q not finished: %v", c.NodeID(), err)
	})

	// Force remove the connection locally in case it exists
	s1.nodeConnsLock.Lock()
	delete(s1.nodeConns, c.NodeID())
	s1.nodeConnsLock.Unlock()

	// Make the request
	req := &cstructs.FsLogsRequest{
		AllocID:      a.ID,
		Task:         a.Job.TaskGroups[0].Tasks[0].Name,
		LogType:      "stdout",
		Origin:       "start",
		PlainText:    true,
		QueryOptions: structs.QueryOptions{Region: "global"},
	}

	// Get the handler
	handler, err := s1.StreamingRpcHandler("FileSystem.Logs")
	require.Nil(err)

	// Create a pipe
	p1, p2 := net.Pipe()
	defer p1.Close()
	defer p2.Close()

	errCh := make(chan error)
	streamMsg := make(chan *cstructs.StreamErrWrapper)

	// Start the handler
	go handler(p2)

	// Start the decoder
	go func() {
		decoder := codec.NewDecoder(p1, structs.MsgpackHandle)
		for {
			var msg cstructs.StreamErrWrapper
			if err := decoder.Decode(&msg); err != nil {
				if err == io.EOF || strings.Contains(err.Error(), "closed") {
					return
				}
				errCh <- fmt.Errorf("error decoding: %v", err)
			}

			streamMsg <- &msg
		}
	}()

	// Send the request
	encoder := codec.NewEncoder(p1, structs.MsgpackHandle)
	require.Nil(encoder.Encode(req))

	timeout := time.After(3 * time.Second)
	received := ""
OUTER:
	for {
		select {
		case <-timeout:
			t.Fatal("timeout")
		case err := <-errCh:
			t.Fatal(err)
		case msg := <-streamMsg:
			if msg.Error != nil {
				t.Fatalf("Got error: %v", msg.Error.Error())
			}

			// Add the payload
			received += string(msg.Payload)
			if received == expected {
				break OUTER
			}
		}
	}
}

func TestClientFS_Logs_Remote_Region(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	// Start a server and client
	s1 := TestServer(t, nil)
	defer s1.Shutdown()
	s2 := TestServer(t, func(c *Config) {
		c.Region = "two"
	})
	defer s2.Shutdown()
	TestJoin(t, s1, s2)
	testutil.WaitForLeader(t, s1.RPC)
	testutil.WaitForLeader(t, s2.RPC)

	c := client.TestClient(t, func(c *config.Config) {
		c.Servers = []string{s2.config.RPCAddr.String()}
		c.Region = "two"
	})
	defer c.Shutdown()

	// Force an allocation onto the node
	expected := "Hello from the other side"
	a := mock.Alloc()
	a.Job.Type = structs.JobTypeBatch
	a.NodeID = c.NodeID()
	a.Job.TaskGroups[0].Count = 1
	a.Job.TaskGroups[0].Tasks[0] = &structs.Task{
		Name:   "web",
		Driver: "mock_driver",
		Config: map[string]interface{}{
			"run_for":       "2s",
			"stdout_string": expected,
		},
		LogConfig: structs.DefaultLogConfig(),
		Resources: &structs.Resources{
			CPU:      500,
			MemoryMB: 256,
		},
	}

	// Wait for the client to connect
	testutil.WaitForResult(func() (bool, error) {
		nodes := s2.connectedNodes()
		return len(nodes) == 1, nil
	}, func(err error) {
		t.Fatalf("should have a client")
	})

	// Upsert the allocation
	state2 := s2.State()
	require.Nil(state2.UpsertJob(999, a.Job))
	require.Nil(state2.UpsertAllocs(1003, []*structs.Allocation{a}))

	// Wait for the client to run the allocation
	testutil.WaitForResult(func() (bool, error) {
		alloc, err := state2.AllocByID(nil, a.ID)
		if err != nil {
			return false, err
		}
		if alloc == nil {
			return false, fmt.Errorf("unknown alloc")
		}
		if alloc.ClientStatus != structs.AllocClientStatusComplete {
			return false, fmt.Errorf("alloc client status: %v", alloc.ClientStatus)
		}

		return true, nil
	}, func(err error) {
		t.Fatalf("Alloc on node %q not finished: %v", c.NodeID(), err)
	})

	// Make the request
	req := &cstructs.FsLogsRequest{
		AllocID:      a.ID,
		Task:         a.Job.TaskGroups[0].Tasks[0].Name,
		LogType:      "stdout",
		Origin:       "start",
		PlainText:    true,
		QueryOptions: structs.QueryOptions{Region: "two"},
	}

	// Get the handler
	handler, err := s1.StreamingRpcHandler("FileSystem.Logs")
	require.Nil(err)

	// Create a pipe
	p1, p2 := net.Pipe()
	defer p1.Close()
	defer p2.Close()

	errCh := make(chan error)
	streamMsg := make(chan *cstructs.StreamErrWrapper)

	// Start the handler
	go handler(p2)

	// Start the decoder
	go func() {
		decoder := codec.NewDecoder(p1, structs.MsgpackHandle)
		for {
			var msg cstructs.StreamErrWrapper
			if err := decoder.Decode(&msg); err != nil {
				if err == io.EOF || strings.Contains(err.Error(), "closed") {
					return
				}
				errCh <- fmt.Errorf("error decoding: %v", err)
			}

			streamMsg <- &msg
		}
	}()

	// Send the request
	encoder := codec.NewEncoder(p1, structs.MsgpackHandle)
	require.Nil(encoder.Encode(req))

	timeout := time.After(3 * time.Second)
	received := ""
OUTER:
	for {
		select {
		case <-timeout:
			t.Fatal("timeout")
		case err := <-errCh:
			t.Fatal(err)
		case msg := <-streamMsg:
			if msg.Error != nil {
				t.Fatalf("Got error: %v", msg.Error.Error())
			}

			// Add the payload
			received += string(msg.Payload)
			if received == expected {
				break OUTER
			}
		}
	}
}
