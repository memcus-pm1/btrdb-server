package bstoreGen1

import (
	"errors"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
	"log"
	"sync"
	"code.google.com/p/go-uuid/uuid"
	"os"
	"fmt"
)

const LatestGeneration = uint64(^(uint64(0)))

const KFACTOR = 64
const PWFACTOR = uint8(6) //1<<6 == 64
const VSIZE = 256
const FNUM = 8
const BALLOC_INC = 4096 //How many blocks to prealloc
const OFFSET_MASK = 0x0000FFFFFFFFFFFF
func UUIDToMapKey(id uuid.UUID) [16]byte {
	rv := [16]byte{}
	copy(rv[:], id)
	return rv
}
func init() {
	log.SetFlags( log.Ldate | log.Lmicroseconds | log.Lshortfile )
}


type BlockStore struct {
	ses    *mgo.Session
	db     *mgo.Database
	_wlocks map[[16]byte]*sync.Mutex
	glock  sync.RWMutex
	
	fidx		chan int
	maxblock	[]uint64
	nxtblock	[]uint64
	dbf 		[]*os.File
	blockmtx	[]sync.Mutex
}


var block_buf_pool = sync.Pool{
	New: func() interface{} {
		return make([]byte, DBSIZE)
	},
}

var core_pool = sync.Pool{
	New: func() interface{} {
		return new(Coreblock)
	},
}
var vector_pool = sync.Pool {
	New: func() interface{} {
		return new(Vectorblock)
	},
}

var ErrDatablockNotFound = errors.New("Coreblock not found")

/* A generation stores all the information acquired during a write pass.
 * A superblock contains all the information required to navigate a tree.
 */
type Generation struct {
	Cur_SB  	*Superblock
	New_SB		*Superblock
	cblocks    	[]*Coreblock
	vblocks    	[]*Vectorblock
	blockstore 	*BlockStore
	flushed    	bool
}

func (g *Generation) UpdateRootAddr(addr uint64) {
	//log.Printf("updateaddr called (%v)",addr)
	g.New_SB.root = addr
}
func (g *Generation) Uuid() *uuid.UUID {
	return &g.Cur_SB.uuid
}

func (g *Generation) Number() uint64 {
	return g.New_SB.gen
}

//This is called with the correct mutex locked
func (bs *BlockStore) expandDB(i int) error {
	err := bs.dbf[i].Truncate(int64((bs.maxblock[i] + BALLOC_INC)*DBSIZE))
	bs.maxblock[i] += BALLOC_INC
	if err != nil {
		log.Panic(err)
	}
	return err
}

func NewBlockStore (targetserv string, cachesize uint64, dbpath string) (*BlockStore, error) {
	bs := BlockStore{}
	ses, err := mgo.Dial(targetserv)
	if err != nil {
		return nil, err
	}
	bs.ses = ses
	bs.db = ses.DB("quasar")
	bs._wlocks = make(map[[16]byte]*sync.Mutex)
	
	bs.maxblock = make([]uint64, FNUM)
	bs.nxtblock = make([]uint64, FNUM)
	bs.dbf = make([]*os.File, FNUM)
	bs.blockmtx = make([]sync.Mutex,FNUM)
	bs.fidx = make(chan int)
	go func() {
		idx := 0
		for {
			bs.fidx <- idx
			idx += 1
			if idx == FNUM {
				idx = 0
			}
		}
	}()
	
	for fi := 0; fi < FNUM; fi++ {
		fname := fmt.Sprintf("/%s/blockstore.%02x.db", dbpath, fi)
		f, err := os.OpenFile(fname, os.O_RDWR | os.O_CREATE, 0666)
		if err != nil {
			log.Printf("Problem with blockstore DB")
			log.Panic(err)
		}
		l, err := f.Seek(0, os.SEEK_END) 
		if err != nil {
			log.Panic(err)
		}
		if l & (DBSIZE-1) != 0 {
			log.Printf("dbsize is: %v", l)
			log.Panic("DB is weird size")
		}
		bs.dbf[fi] = f
		bs.nxtblock[fi] = uint64(l/DBSIZE)
		bs.maxblock[fi] = uint64(l/DBSIZE)
		bs.expandDB(fi)
		if (bs.nxtblock[fi] == 0) { //0 is reserved for invalid address
			bs.nxtblock[fi] = 1
		}
		f.Sync()
	}
	return &bs, nil
}

/*
 * This obtains a generation, blocking if necessary
 */
func (bs *BlockStore) ObtainGeneration(id uuid.UUID) *Generation {
	//The first thing we do is obtain a write lock on the UUID, as a generation
	//represents a lock
	mk := UUIDToMapKey(id)
	bs.glock.RLock()
	mtx, ok := bs._wlocks[mk]
	bs.glock.RUnlock()
	if !ok {
		//Mutex doesn't exist so is unlocked
		mtx := new(sync.Mutex)
		mtx.Lock()
		bs.glock.Lock()
		bs._wlocks[mk] = mtx
		bs.glock.Unlock()
	} else {
		mtx.Lock()
	}
	
	gen := &Generation{
		cblocks: make([]*Coreblock, 0, 32),
		vblocks: make([]*Vectorblock, 0, 32),
	}
	//We need a generation. Lets see if one is on disk
	qry := bs.db.C("superblocks").Find(bson.M{"uuid": id.String()})
	rs := fake_sblock{}
	qerr := qry.Sort("-gen").One(&rs)
	if qerr == mgo.ErrNotFound {
		log.Printf("no superblock found for %v", id.String())
		//Ok just create a new superblock/generation
		gen.Cur_SB = NewSuperblock(id)
		//No we don't want to put it in the DB. It doesn't even have a root!
		/*rs := fake_sblock { //yes I know we have all this
			Uuid : gen.Cur_SB.uuid,
			Gen : gen.Cur_SB.gen,
			Root : gen.Cur_SB.root,
		}
		//Put it in the DB
		if err := bs.db.C("superblocks").Insert(&rs); err != nil {
			log.Panic(err)
		}*/
	} else if qerr != nil {
		//Well thats more serious
		log.Panic(qerr)
	} else {
		//Ok we have a superblock, pop the gen
		log.Printf("Found a superblock for %v", id.String())
		sb := Superblock {
			uuid : id,
			root : rs.Root,
			gen : rs.Gen,
		}
		gen.Cur_SB = &sb
	}
	
	gen.New_SB = gen.Cur_SB.Clone()
	gen.New_SB.gen = gen.Cur_SB.gen + 1
	gen.blockstore = bs
	return gen
}

func (gen *Generation) Commit() error {
	if gen.flushed {
		return errors.New("Already Flushed")
	}
	reqf := make(map[int]bool)
	for _, cb := range gen.cblocks {
		reqf[int(cb.This_addr >> 48)] = true
		gen.blockstore.writeCoreblockAndFree(cb)
	}
	gen.cblocks = nil
	for _, vb := range gen.vblocks {
		reqf[int(vb.This_addr >> 48)] = true
		gen.blockstore.writeVectorblockAndFree(vb)
	}
	gen.vblocks = nil
	for fi := range reqf {
		gen.blockstore.datablockBarrier(fi)
	}
	log.Printf("inserting supeblock u=%v gen=%v root=%v", gen.Uuid().String(), gen.Number(), gen.New_SB.root) 
	//Ok we cannot directly write a superblock to the DB here
	fsb := fake_sblock {
		Uuid : gen.New_SB.uuid.String(),
		Gen : gen.New_SB.gen,
		Root : gen.New_SB.root,
	}
	if err := gen.blockstore.db.C("superblocks").Insert(fsb); err != nil {
		log.Panic(err)
	}
	gen.flushed = true
	gen.blockstore.glock.RLock()
	//log.Printf("bs is %v, wlocks is %v", gen.blockstore, gen.blockstore._wlocks)
	gen.blockstore._wlocks[UUIDToMapKey(*gen.Uuid())].Unlock()
	gen.blockstore.glock.RUnlock()
	return nil
}

func (bs *BlockStore) datablockBarrier(fi int) {
	//Gonuts group says that I don't need to call Sync()
	
	//Block until all datablocks have finished writing
	/*bs.blockmtx[fi].Lock()
	err := bs.dbf[fi].Sync()
	if err != nil {
		log.Panic(err)
	}
	bs.blockmtx[fi].Unlock()*/
	//bs.ses.Fsync(false)
}

func (bs *BlockStore) allocateBlock() uint64 {
	//TODO the real system will have an allocator that makes
	//unique 64 bit addresses. We don't have tha yet, so we
	//try make one from the mongo ID and timestamp
	/*id := bson.NewObjectId()
	cnt := id.Counter()
	tm := uint64(id.Time().Unix() & 0xFFFFFFFF)
	addr := (tm << 32) + (uint64(cnt) & 0xFFFFFFFF)
	addr &= 0x7FFFFFFFFFFFFFFF*/
	
	//AHAHA the previous was a terrible idea, as mongo's id.Counter() is per
	//instance and we kept getting colissions. yaaay.
	//This will do for now, as long as somebody seeds the random number gen
	
	rr := <- bs.fidx
	
	bs.blockmtx[rr].Lock()
	rv := bs.nxtblock[rr]
	bs.nxtblock[rr] += 1
	if bs.nxtblock[rr] == bs.maxblock[rr] {
		bs.expandDB(rr)
	}
	bs.blockmtx[rr].Unlock()
	//Encode the fidx in the top 16 bits
	return rv | (uint64(rr) << 48) 
}
/**
 * The real function is supposed to allocate an address for the data
 * block, reserving it on disk, and then give back the data block that
 * can be filled in
 * This stub makes up an address, and mongo pretends its real
 */
func (gen *Generation) AllocateCoreblock() (*Coreblock, error) {
	cblock := core_pool.Get().(*Coreblock)
	*cblock = ZeroCoreblock
	cblock.This_addr = gen.blockstore.allocateBlock()
	cblock.Generation = gen.Number()
	gen.cblocks = append(gen.cblocks, cblock)
	return cblock, nil
}

func (gen *Generation) AllocateVectorblock() (*Vectorblock, error) {
	vblock := vector_pool.Get().(*Vectorblock)
	*vblock = ZeroVectorblock
	vblock.This_addr = gen.blockstore.allocateBlock()
	vblock.Generation = gen.Number()
	gen.vblocks = append(gen.vblocks, vblock)
	return vblock, nil
}

func (bs *BlockStore) FreeCoreblock(cb **Coreblock) {
	core_pool.Put(*cb)
	*cb = nil
}

func (bs *BlockStore) FreeVectorblock(vb **Vectorblock) {
	vector_pool.Put(*vb)
	*vb = nil
}
type fake_dblock_t struct {
	Addr uint64
	Data []byte
}

func (bs *BlockStore) DEBUG_DELETE_UUID(id uuid.UUID) {
	log.Printf("DEBUG removing uuid '%v' from database", id.String()) 
	_, err := bs.db.C("superblocks").RemoveAll(bson.M{"uuid":id.String()})
	if err != nil && err != mgo.ErrNotFound {
		log.Panic(err)
	}
	if err == mgo.ErrNotFound {
		log.Printf("Quey did not find supeblock to delete")
	} else {
		log.Printf("err was nik")
	}
	//bs.datablockBarrier()
}

func (bs *BlockStore) writeDBlock(addr uint64, contents []byte) error {
	rr := addr >> 48
	addr &= OFFSET_MASK
	bs.blockmtx[rr].Lock()
	_, err := bs.dbf[rr].WriteAt(contents, int64(addr * DBSIZE))
	bs.blockmtx[rr].Unlock()
	return err
}

func (bs *BlockStore) readDBlock(addr uint64, buf []byte) (error) {
	rr := addr >> 48
	addr &= OFFSET_MASK
	bs.blockmtx[rr].Lock()
	_, err := bs.dbf[rr].ReadAt(buf, int64(addr*DBSIZE))
	bs.blockmtx[rr].Unlock()
	return err
}
/**
 * The real function is meant to now write back the contents
 * of the data block to the address. This just uses the address
 * as a key
 */
func (bs *BlockStore) writeCoreblockAndFree(cb *Coreblock) error {
	syncbuf := block_buf_pool.Get().([]byte)
	cb.Serialize(syncbuf)
	ierr := bs.writeDBlock(cb.This_addr, syncbuf)
	if ierr != nil {
		log.Panic(ierr)
	}
	block_buf_pool.Put(syncbuf)
	core_pool.Put(cb)
	return nil
}

func (bs *BlockStore) writeVectorblockAndFree(vb *Vectorblock) error {
	syncbuf := block_buf_pool.Get().([]byte)
	vb.Serialize(syncbuf)
	ierr := bs.writeDBlock(vb.This_addr, syncbuf)
	if ierr != nil {
		log.Panic(ierr)
	}
	block_buf_pool.Put(syncbuf)
	vector_pool.Put(vb)
	return nil
}

func (bs *BlockStore) ReadDatablock(addr uint64) (Datablock, error) {
	syncbuf := block_buf_pool.Get().([]byte)
	err := bs.readDBlock(addr, syncbuf)
	if err != nil {
		log.Panic(err)
	}
	switch DatablockGetBufferType(syncbuf) {
	case Core:
		rv := core_pool.Get().(*Coreblock)
		rv.Deserialize(syncbuf)
		block_buf_pool.Put(syncbuf)
		return rv, nil
	case Vector:
		rv := vector_pool.Get().(*Vectorblock)
		rv.Deserialize(syncbuf)
		block_buf_pool.Put(syncbuf)
		return rv, nil
	}
	log.Panic("Strange datablock type")
	return nil, nil
}

type fake_sblock struct {
	Uuid  string
	Gen uint64
	Root  uint64
}
func (bs *BlockStore) LoadSuperblock(id uuid.UUID, generation uint64) (*Superblock) {
	var sb = fake_sblock{}
	if generation == LatestGeneration {
		log.Printf("loading superblock uuid=%v (lgen)",id.String())
		qry := bs.db.C("superblocks").Find(bson.M{"uuid":id.String()})
		if err := qry.Sort("-gen").One(&sb); err != nil {
			if err == mgo.ErrNotFound {
				log.Printf("sb notfound!")
				return nil
			} else {
				log.Panic(err)
			}
		}
	} else {
		qry := bs.db.C("superblocks").Find(bson.M{"uuid":id.String(),"gen":generation})
		if err := qry.One(&sb); err != nil {
			if err == mgo.ErrNotFound {
				return nil
			} else {
				log.Panic(err)
			}
		}		
	}
	rv := Superblock{
		uuid: id,
		gen : sb.Gen,
		root : sb.Root,
	}
	return &rv
}

