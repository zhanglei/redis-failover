package failover

import (
	"bytes"
	"fmt"
	"github.com/garyburd/redigo/redis"
	. "gopkg.in/check.v1"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func Test(t *testing.T) {
	TestingT(t)
}

type failoverTestSuite struct {
}

var _ = Suite(&failoverTestSuite{})

var testPort = []int{16379, 16380, 16381}

func (s *failoverTestSuite) SetUpSuite(c *C) {
	_, err := exec.LookPath("redis-server")
	c.Assert(err, IsNil)
}

func (s *failoverTestSuite) TearDownSuite(c *C) {

}

func (s *failoverTestSuite) SetUpTest(c *C) {
	for _, port := range testPort {
		s.stopRedis(c, port)
		s.startRedis(c, port)
		s.doCommand(c, port, "SLAVEOF", "NO", "ONE")
		s.doCommand(c, port, "FLUSHALL")
	}
}

func (s *failoverTestSuite) TearDownTest(c *C) {
	for _, port := range testPort {
		s.stopRedis(c, port)
	}
}

type redisChecker struct {
	sync.Mutex
	ok  bool
	buf bytes.Buffer
}

func (r *redisChecker) Write(data []byte) (int, error) {
	r.Lock()
	defer r.Unlock()

	r.buf.Write(data)
	if strings.Contains(r.buf.String(), "The server is now ready to accept connections") {
		r.ok = true
	}

	return len(data), nil
}

func (s *failoverTestSuite) startRedis(c *C, port int) {
	checker := &redisChecker{ok: false}
	// start redis and use memory only
	cmd := exec.Command("redis-server", "--port", fmt.Sprintf("%d", port), "--save", "")
	cmd.Stdout = checker
	cmd.Stderr = checker

	err := cmd.Start()
	c.Assert(err, IsNil)

	for i := 0; i < 20; i++ {
		var ok bool
		checker.Lock()
		ok = checker.ok
		checker.Unlock()

		if ok {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}

	c.Fatal("redis-server can not start ok after 10s")
}

func (s *failoverTestSuite) stopRedis(c *C, port int) {
	cmd := exec.Command("redis-cli", "-p", fmt.Sprintf("%d", port), "shutdown", "nosave")
	cmd.Run()
}

func (s *failoverTestSuite) doCommand(c *C, port int, cmd string, cmdArgs ...interface{}) interface{} {
	conn, err := redis.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	c.Assert(err, IsNil)

	v, err := conn.Do(cmd, cmdArgs...)
	c.Assert(err, IsNil)
	return v
}

func (s *failoverTestSuite) TestSimpleCheck(c *C) {
	cfg := new(Config)
	cfg.Addr = ":11000"

	port := testPort[0]
	cfg.Masters = []string{fmt.Sprintf("127.0.0.1:%d", port)}
	cfg.CheckInterval = 500

	app, err := NewApp(cfg)
	c.Assert(err, IsNil)

	defer app.Close()

	go func() {
		app.Run()
	}()

	s.doCommand(c, port, "SET", "a", 1)
	n, err := redis.Int(s.doCommand(c, port, "GET", "a"), nil)
	c.Assert(err, IsNil)
	c.Assert(n, Equals, 1)

	ch := s.addBeforeHandler(app)

	ms := app.masters.GetMasters()
	c.Assert(ms, DeepEquals, []string{fmt.Sprintf("127.0.0.1:%d", port)})

	s.stopRedis(c, port)

	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		c.Fatal("check is not ok after 5s, too slow")
	}
}

func (s *failoverTestSuite) TestFailoverCheck(c *C) {
	cfg := new(Config)
	cfg.Addr = ":11000"

	port := testPort[0]
	masterAddr := fmt.Sprintf("127.0.0.1:%d", port)

	cfg.Masters = []string{masterAddr}
	cfg.CheckInterval = 500

	app, err := NewApp(cfg)
	c.Assert(err, IsNil)

	defer app.Close()

	ch := s.addAfterHandler(app)

	go func() {
		app.Run()
	}()

	s.buildReplTopo(c)

	s.stopRedis(c, port)

	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		c.Fatal("failover is not ok after 5s, too slow")
	}
}

func (s *failoverTestSuite) TestOneFaftFailoverCheck(c *C) {
	apps := s.newClusterApp(c, 1, 0)
	app := apps[0]

	defer app.Close()

	select {
	case b := <-app.r.LeaderCh():
		c.Assert(b, Equals, true)
	case <-time.After(5 * time.Second):
		c.Fatal("elect to leader failed after 5s, too slow")
	}

	port := testPort[0]
	masterAddr := fmt.Sprintf("127.0.0.1:%d", port)

	err := app.addMasters([]string{masterAddr})
	c.Assert(err, IsNil)

	ch := s.addBeforeHandler(app)

	ms := app.masters.GetMasters()
	c.Assert(ms, DeepEquals, []string{fmt.Sprintf("127.0.0.1:%d", port)})

	s.stopRedis(c, port)

	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		c.Fatal("check is not ok after 5s, too slow")
	}
}

func (s *failoverTestSuite) TestMultiFaftFailoverCheck(c *C) {
	apps := s.newClusterApp(c, 3, 10)
	defer func() {
		for _, app := range apps {
			app.Close()
		}
	}()

	lc := make(chan *App, 3)
	for _, app := range apps {
		go func(app *App) {
			b := <-app.r.LeaderCh()
			if b {
				lc <- app
			}
		}(app)
	}

	var app *App
	// leader
	select {
	case app = <-lc:
	case <-time.After(5 * time.Second):
		c.Fatal("can not elect a leader after 5s, too slow")
	}

	port := testPort[0]
	masterAddr := fmt.Sprintf("127.0.0.1:%d", port)

	err := app.addMasters([]string{masterAddr})
	c.Assert(err, IsNil)

	ch := s.addBeforeHandler(app)

	ms := app.masters.GetMasters()
	c.Assert(ms, DeepEquals, []string{fmt.Sprintf("127.0.0.1:%d", port)})

	s.stopRedis(c, port)

	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		c.Fatal("check is not ok after 5s, too slow")
	}

	ms = app.masters.GetMasters()
	c.Assert(ms, DeepEquals, []string{})

	err = app.r.Barrier(5 * time.Second)
	c.Assert(err, IsNil)

	// close leader
	app.Close()

	// start redis
	s.startRedis(c, port)

	// wait other two elect new leader

	lc = make(chan *App, 3)
	for _, a := range apps {
		if a == app {
			continue
		}
		go func(app *App) {
			b := <-app.r.LeaderCh()
			if b {
				lc <- app
			}
		}(a)
	}

	select {
	case app = <-lc:
	case <-time.After(5 * time.Second):
		c.Fatal("can not elect a leader after 5s, too slow")
	}

	err = app.addMasters([]string{masterAddr})
	c.Assert(err, IsNil)

	ch = s.addBeforeHandler(app)

	ms = app.masters.GetMasters()
	c.Assert(ms, DeepEquals, []string{fmt.Sprintf("127.0.0.1:%d", port)})

	s.stopRedis(c, port)

	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		c.Fatal("check is not ok after 5s, too slow")
	}
}

func (s *failoverTestSuite) addBeforeHandler(app *App) chan string {
	ch := make(chan string, 1)
	f := func(downMaster string) error {
		ch <- downMaster
		return nil
	}

	app.AddBeforeFailoverHandler(f)
	return ch
}

func (s *failoverTestSuite) addAfterHandler(app *App) chan string {
	ch := make(chan string, 1)
	f := func(oldMaster string, newMaster string) error {
		ch <- newMaster
		return nil
	}

	app.AddAfterFailoverHandler(f)
	return ch
}

func (s *failoverTestSuite) newClusterApp(c *C, num int, base int) []*App {
	port := 11000
	raftPort := 12000
	cluster := make([]string, 0, num)
	for i := 0; i < num; i++ {
		cluster = append(cluster, fmt.Sprintf("127.0.0.1:%d", raftPort+i+base))
	}
	apps := make([]*App, 0, num)

	os.RemoveAll("./var")

	for i := 0; i < num; i++ {
		cfg := new(Config)
		cfg.Addr = fmt.Sprintf(":%d", port+i)

		cfg.Raft.Addr = fmt.Sprintf("127.0.0.1:%d", raftPort+i+base)
		cfg.Raft.DataDir = fmt.Sprintf("./var/store/%d", i+base)
		cfg.Raft.LogDir = fmt.Sprintf("./var/log/%d", i+base)

		cfg.Raft.ClusterState = ClusterStateExisting
		cfg.Raft.Cluster = cluster

		app, err := NewApp(cfg)

		c.Assert(err, IsNil)
		go func() {
			app.Run()
		}()

		apps = append(apps, app)
	}

	return apps
}

func (s *failoverTestSuite) buildReplTopo(c *C) {
	port := testPort[0]

	s.doCommand(c, testPort[1], "SLAVEOF", "127.0.0.1", port)
	s.doCommand(c, testPort[2], "SLAVEOF", "127.0.0.1", port)

	s.doCommand(c, port, "SET", "a", 10)
	s.doCommand(c, port, "SET", "b", 20)

	s.waitReplConnected(c, testPort[1], 10)
	s.waitReplConnected(c, testPort[2], 10)

	s.waitSync(c, port, 10)

	n, err := redis.Int(s.doCommand(c, testPort[1], "GET", "a"), nil)
	c.Assert(err, IsNil)
	c.Assert(n, Equals, 10)

	n, err = redis.Int(s.doCommand(c, testPort[2], "GET", "a"), nil)
	c.Assert(err, IsNil)
	c.Assert(n, Equals, 10)
}

func (s *failoverTestSuite) waitReplConnected(c *C, port int, timeout int) {
	for i := 0; i < timeout*2; i++ {
		v, _ := redis.Values(s.doCommand(c, port, "ROLE"), nil)
		tp, _ := redis.String(v[0], nil)
		if tp == SlaveType {
			state, _ := redis.String(v[3], nil)
			if state == ConnectedState || state == SyncState {
				return
			}
		}

		time.Sleep(500 * time.Millisecond)
	}

	c.Fatalf("wait %ds, but 127.0.0.1:%d can not connect to master", timeout, port)
}

func (s *failoverTestSuite) waitSync(c *C, port int, timeout int) {
	g := newGroup(fmt.Sprintf("127.0.0.1:%d", port))

	for i := 0; i < timeout*2; i++ {
		err := g.doRole()
		c.Assert(err, IsNil)

		same := true
		offset := g.Master.Offset
		if offset > 0 {
			for _, slave := range g.Slaves {
				if slave.Offset != offset {
					same = false
				}
			}
		}

		if same {
			return
		}

		time.Sleep(500 * time.Millisecond)
	}

	c.Fatalf("wait %ds, but all slaves can not sync the same with master %v", timeout, g)
}