// This project is licensed under the MIT License (see LICENSE).

package petri

import (
    "sync"
    "sync/atomic"
    "time"
)

type Env struct {
    Width int32
    Height int32
    GenomeSize int32
    Seed int64

    initPop int32

    config atomic.Value
    rng atomic.Value

    mutex *sync.RWMutex
    cells []*Cell
    cellsBuf []int32
    liveCells map[int32]bool

    run chan bool
    nextCellID chan int64
}

type Config struct {
    InflowFrequency int64
    ViableCellGeneration int64
    FailedKillPenalty int
    SeedLiveCells bool
}

const (
    DIR_LEFT int = iota
    DIR_RIGHT
    DIR_UP
    DIR_DOWN
)

var defaultConfig = Config{
    InflowFrequency: 10,
    ViableCellGeneration: 3,
    FailedKillPenalty: 3,
    SeedLiveCells: false,
}

func NewEnv(width, height, genomeSize, pop int32, seed int64) *Env {
    e := &Env{
        Width: width,
        Height: height,
        GenomeSize: genomeSize,
        Seed: seed,
        initPop: pop,
        mutex: &sync.RWMutex{},
        cells: make([]*Cell, width * height),
        cellsBuf: make([]int32, width * height),
        liveCells: make(map[int32]bool),
        run: make(chan bool),
        nextCellID: make(chan int64),
    }

    if seed < 1 {
        e.Seed = time.Now().UnixNano()
    }

    for i := range e.cells {
        idx := int32(i)
        x := idx % width
        y := idx / width
        e.cells[i] = newCell(idx, x, y, genomeSize)
    }

    e.SetConfig(defaultConfig)
    e.SetRNG(defaultRNG)

    return e
}

func (e *Env) GetConfig() Config {
    return e.config.Load().(Config)
}

func (e *Env) SetConfig(c Config) {
    e.config.Store(c)
}

func (e *Env) GetRNG() RNG {
    return e.rng.Load().(RNG)
}

func (e *Env) SetRNG(r RNG) {
    e.rng.Store(r)
}

func (e *Env) getNextCellID() int64 {
    return <-e.nextCellID
}

func (e *Env) applyDelta(dt *Delta) {
    e.mutex.Lock()
    for _, c := range dt.Cells {
        if c.live() {
            e.liveCells[c.idx] = true
        } else {
            delete(e.liveCells, c.idx)
        }
        e.cells[c.idx] = c.clone()
    }
    e.mutex.Unlock()
}

func (e *Env) GetCell(x, y int32) *Cell {
    e.mutex.RLock()
    defer e.mutex.RUnlock()
    return e.cells[x + e.Width * y].clone()
}

func (e *Env) getRandomCell(ctx *Context) *Cell {
    x := ctx.rand.Int31n(ctx.env.Width)
    y := ctx.rand.Int31n(ctx.env.Height)
    return e.GetCell(x, y)
}

func (e *Env) getRandomLiveCell(ctx *Context) *Cell {
    e.mutex.RLock()
    defer e.mutex.RUnlock()

    i := 0
    for idx := range e.liveCells {
        e.cellsBuf[i] = idx
        i++
    }

    if i == 0 {
        return nil
    }

    c := e.cellsBuf[ctx.rand.Intn(i)]

    return e.cells[c].clone()
}

func (e *Env) getRandomDeadCell(ctx *Context) *Cell {
    e.mutex.RLock()
    defer e.mutex.RUnlock()

    i := 0
    for _, c := range e.cells {
        if _, live := e.liveCells[c.idx]; !live {
            e.cellsBuf[i] = c.idx
            i++
        }
    }

    if i == 0 {
        return nil
    }

    c := e.cellsBuf[ctx.rand.Intn(i)]

    return e.cells[c].clone()
}

func (e *Env) getNeighbor(c *Cell, dir int) *Cell {
    x, y := c.X, c.Y

    switch dir {
    case DIR_LEFT:
        if x == 0 {
            x = e.Width - 1
        } else {
            x--
        }
    case DIR_RIGHT:
        if x == e.Width - 1 {
            x = 0
        } else {
            x++
        }
    case DIR_UP:
        if y == 0 {
            y = e.Height - 1
        } else {
            y--
        }
    case DIR_DOWN:
        if y == e.Height - 1 {
            y = 0
        } else {
            y++
        }
    }

    return e.GetCell(x, y)
}

func (e *Env) process(wg *sync.WaitGroup, exec <-chan bool, inflow chan bool,
    dts chan<- *Delta) {
    defer wg.Done()

    ctx := newContext(e)

    for {
        select {
        case <-inflow:
            var c *Cell
            if !e.GetConfig().SeedLiveCells {
                if c = e.getRandomDeadCell(ctx); c == nil {
                    break
                }
            } else {
                c = e.getRandomCell(ctx)
            }
            dts <- c.seed(ctx)
        case _, ok := <-exec:
            if !ok {
                return
            }
            if c := e.getRandomLiveCell(ctx); c != nil {
                dts <- c.exec(ctx)
            } else {
                go func() {
                    inflow <- true
                }()
            }
        }
    }
}

func (e *Env) Run(processN int, tick time.Duration, deltas chan<- *Delta) {
    exec := make(chan bool)
    inflow := make(chan bool)
    dts := make(chan *Delta, processN)

    var wg sync.WaitGroup
    wg.Add(processN)

    for i := 0; i < processN; i++ {
        go e.process(&wg, exec, inflow, dts)
    }

    go func() {
        defer close(e.nextCellID)
        var id int64 = 1
        for {
            e.nextCellID <- id
            id++
        }
    }()

    defer close(inflow)
    defer close(dts)
    defer close(deltas)

    for e.initPop > 0 {
        inflow <- true
        e.initPop--
    }

    ticker := time.NewTicker(tick)
    defer ticker.Stop()

    inflowTick := e.GetConfig().InflowFrequency
    running := true

    for running {
        select {
        case _, ok := <-e.run:
            if !ok {
                close(exec)
                running = false
            }
        case <-ticker.C:
            inflowTick--
            if inflowTick == 0 {
                inflow <- true
                inflowTick = e.GetConfig().InflowFrequency
            }
            exec <- true
        case dt := <-dts:
            e.applyDelta(dt)
            deltas <- dt
        }
    }

    wg.Wait()
}

func (e *Env) Stop() {
    close(e.run)
}
