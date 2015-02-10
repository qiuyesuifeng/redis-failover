package failover

import (
	"github.com/gorilla/mux"
	"github.com/siddontang/go/log"
	"net"
	"net/http"
	"sync"
	"time"
)

type BeforeFailoverHandler func(downMaster string) error
type AfterFailoverHandler func(downMaster, newMaster string) error

type App struct {
	c *Config

	l net.Listener

	r *Raft

	masters *masterFSM

	gMutex sync.Mutex
	groups map[string]*Group

	quit chan struct{}
	wg   sync.WaitGroup

	hMutex         sync.Mutex
	beforeHandlers []BeforeFailoverHandler
	afterHandlers  []AfterFailoverHandler
}

func NewApp(c *Config) (*App, error) {
	var err error

	a := new(App)
	a.c = c
	a.quit = make(chan struct{})
	a.groups = make(map[string]*Group)

	a.masters = newMasterFSM()

	if len(c.Addr) > 0 {
		a.l, err = net.Listen("tcp", c.Addr)
		if err != nil {
			return nil, err
		}
	}

	a.r, err = newRaft(c, a.masters)
	if err != nil {
		return nil, err
	}

	if c.MastersState == MastersStateNew {
		a.setMasters(c.Masters)
	} else {
		a.addMasters(c.Masters)
	}

	return a, nil
}

func (a *App) Close() {
	select {
	case <-a.quit:
		return
	default:
		break
	}

	if a.l != nil {
		a.l.Close()
	}

	if a.r != nil {
		a.r.Close()
	}

	close(a.quit)

	a.wg.Wait()
}

func (a *App) Run() {
	go a.startHTTP()

	if a.c.CheckInterval <= 0 {
		a.c.CheckInterval = 1000
	}

	a.wg.Add(1)
	t := time.NewTicker(time.Duration(a.c.CheckInterval) * time.Millisecond)
	defer func() {
		t.Stop()
		a.wg.Done()
	}()

	for {
		select {
		case <-t.C:
			a.check()
		case <-a.quit:
			return
		}
	}
}

func (a *App) check() {
	if a.r != nil && !a.r.IsLeader() {
		// is not leader, not check
		return
	}

	masters := a.masters.GetMasters()

	var wg sync.WaitGroup
	for _, master := range masters {
		a.gMutex.Lock()
		g, ok := a.groups[master]
		if !ok {
			g = newGroup(master)
			a.groups[master] = g
		}
		a.gMutex.Unlock()

		wg.Add(1)
		go a.checkMaster(&wg, g)
	}

	a.gMutex.Lock()
	for master, g := range a.groups {
		if !a.masters.IsMaster(master) {
			delete(a.groups, master)
			g.Close()
		}
	}
	a.gMutex.Unlock()
}

func (a *App) checkMaster(wg *sync.WaitGroup, g *Group) {
	defer wg.Done()

	// later, add check strategy, like check failed n numbers in n seconds and do failover, etc.
	// now only check once.
	err := g.Check()
	if err == nil {
		return
	}

	oldMaster := g.Master.Addr

	// If check error, we will remove it from saved masters and not check.
	// I just want to avoid some errors if below failover failed, at that time,
	// handling it manually seems a better way.
	// If you want to recheck it, please add it again.
	a.delMasters([]string{oldMaster})

	if err == ErrNodeType {
		log.Errorf("server %s is not master now, we will skip it", oldMaster)
		return
	}

	log.Errorf("check master %s err %v, do failover", oldMaster, err)

	a.onBeforeFailover(oldMaster)

	// first elect a candidate
	newMaster, err := g.Elect()
	if err != nil {
		// elect error
		return
	}

	log.Errorf("master is down, elect %s as new master, do failover", newMaster)

	// promote the candiate to master
	err = g.Promote(newMaster)

	if err != nil {
		log.Fatalf("do master %s failover err: %v", oldMaster, err)
		return
	}

	a.onAfterFailover(oldMaster, newMaster)
}

func (a *App) startHTTP() {
	if a.l == nil {
		return
	}

	m := mux.NewRouter()

	m.Handle("/master", &masterHandler{a})

	s := http.Server{
		Handler: m,
	}

	s.Serve(a.l)
}

func (a *App) addMasters(addrs []string) error {
	if a.r != nil {
		return a.r.AddMasters(addrs, 0)
	} else {
		a.masters.AddMasters(addrs)
		return nil
	}
}

func (a *App) delMasters(addrs []string) error {
	if a.r != nil {
		return a.r.DelMasters(addrs, 0)
	} else {
		a.masters.DelMasters(addrs)
		return nil
	}
}

func (a *App) setMasters(addrs []string) error {
	if a.r != nil {
		return a.r.SetMasters(addrs, 0)
	} else {
		a.masters.SetMasters(addrs)
		return nil
	}

}

func (a *App) AddBeforeFailoverHandler(f BeforeFailoverHandler) {
	a.hMutex.Lock()
	a.beforeHandlers = append(a.beforeHandlers, f)
	a.hMutex.Unlock()
}

func (a *App) AddAfterFailoverHandler(f AfterFailoverHandler) {
	a.hMutex.Lock()
	a.afterHandlers = append(a.afterHandlers, f)
	a.hMutex.Unlock()
}

func (a *App) onBeforeFailover(downMaster string) {
	a.hMutex.Lock()
	for _, h := range a.beforeHandlers {
		if err := h(downMaster); err != nil {
			log.Errorf("do before failover handler for %s err: %v", downMaster, err)
		}
	}
	a.hMutex.Unlock()
}

func (a *App) onAfterFailover(downMaster string, newMaster string) {
	a.hMutex.Lock()
	for _, h := range a.afterHandlers {
		if err := h(downMaster, newMaster); err != nil {
			log.Errorf("do after failover handler for %s -> %s err: %v", downMaster, newMaster, err)
		}
	}

	a.hMutex.Unlock()
}
