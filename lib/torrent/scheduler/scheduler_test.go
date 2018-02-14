package scheduler

import (
	"os"
	"sync"
	"testing"
	"time"

	"code.uber.internal/infra/kraken/core"
	"code.uber.internal/infra/kraken/lib/serverset"
	"code.uber.internal/infra/kraken/lib/torrent/networkevent"
	"code.uber.internal/infra/kraken/lib/torrent/storage"
	"code.uber.internal/infra/kraken/tracker/announceclient"
	"code.uber.internal/infra/kraken/utils/memsize"

	"github.com/andres-erbsen/clock"
	"github.com/stretchr/testify/require"
	"github.com/uber-go/tally"
)

func TestDownloadTorrentWithSeederAndLeecher(t *testing.T) {
	require := require.New(t)

	mocks, cleanup := newTestMocks(t)
	defer cleanup()

	config := configFixture()

	seeder := mocks.newPeer(config)
	leecher := mocks.newPeer(config)

	tf := core.TestTorrentFileFixture()

	mocks.metaInfoClient.EXPECT().Download(
		namespace, tf.MetaInfo.Name()).Return(tf.MetaInfo, nil).Times(2)

	seeder.writeTorrent(tf)
	require.NoError(<-seeder.scheduler.AddTorrent(namespace, tf.MetaInfo.Name()))

	require.NoError(<-leecher.scheduler.AddTorrent(namespace, tf.MetaInfo.Name()))
	leecher.checkTorrent(t, tf)
}

func TestDownloadManyTorrentsWithSeederAndLeecher(t *testing.T) {
	require := require.New(t)

	mocks, cleanup := newTestMocks(t)
	defer cleanup()

	config := configFixture()

	seeder := mocks.newPeer(config)
	leecher := mocks.newPeer(config)

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		tf := core.TestTorrentFileFixture()

		mocks.metaInfoClient.EXPECT().Download(
			namespace, tf.MetaInfo.Name()).Return(tf.MetaInfo, nil).Times(2)

		wg.Add(1)
		go func() {
			defer wg.Done()

			seeder.writeTorrent(tf)
			require.NoError(<-seeder.scheduler.AddTorrent(namespace, tf.MetaInfo.Name()))

			require.NoError(<-leecher.scheduler.AddTorrent(namespace, tf.MetaInfo.Name()))
			leecher.checkTorrent(t, tf)
		}()
	}
	wg.Wait()
}

func TestDownloadManyTorrentsWithSeederAndManyLeechers(t *testing.T) {
	require := require.New(t)

	mocks, cleanup := newTestMocks(t)
	defer cleanup()

	config := configFixture()

	seeder := mocks.newPeer(config)
	leechers := mocks.newPeers(5, config)

	// Start seeding each torrent.
	torrentFiles := make([]*core.TestTorrentFile, 5)
	for i := range torrentFiles {
		tf := core.TestTorrentFileFixture()
		torrentFiles[i] = tf

		mocks.metaInfoClient.EXPECT().Download(
			namespace, tf.MetaInfo.Name()).Return(tf.MetaInfo, nil).Times(6)

		seeder.writeTorrent(tf)
		require.NoError(<-seeder.scheduler.AddTorrent(namespace, tf.MetaInfo.Name()))
	}

	var wg sync.WaitGroup
	for _, tf := range torrentFiles {
		tf := tf
		for _, p := range leechers {
			p := p
			wg.Add(1)
			go func() {
				defer wg.Done()
				select {
				case err := <-p.scheduler.AddTorrent(namespace, tf.MetaInfo.Name()):
					require.NoError(err)
					p.checkTorrent(t, tf)
				case <-time.After(10 * time.Second):
					t.Errorf("AddTorrent timeout scheduler=%s torrent=%s", p.pctx.PeerID, tf.MetaInfo.InfoHash)
				}
			}()
		}
	}
	wg.Wait()
}

func TestDownloadTorrentWhenPeersAllHaveDifferentPiece(t *testing.T) {
	require := require.New(t)

	mocks, cleanup := newTestMocks(t)
	defer cleanup()

	config := configFixture()

	peers := mocks.newPeers(10, config)

	pieceLength := 256
	tf := core.CustomTestTorrentFileFixture(uint64(len(peers)*pieceLength), uint64(pieceLength))

	mocks.metaInfoClient.EXPECT().Download(
		namespace, tf.MetaInfo.Name()).Return(tf.MetaInfo, nil).Times(len(peers))

	var wg sync.WaitGroup
	for i, p := range peers {
		tor, err := p.torrentArchive.CreateTorrent(namespace, tf.MetaInfo.Name())
		require.NoError(err)

		piece := make([]byte, pieceLength)
		start := i * pieceLength
		stop := (i + 1) * pieceLength
		copy(piece, tf.Content[start:stop])
		require.NoError(tor.WritePiece(storage.NewPieceReaderBuffer(piece), i))

		p := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case err := <-p.scheduler.AddTorrent(namespace, tf.MetaInfo.Name()):
				require.NoError(err)
				p.checkTorrent(t, tf)
			case <-time.After(10 * time.Second):
				t.Errorf("AddTorrent timeout scheduler=%s torrent=%s", p.pctx.PeerID, tf.MetaInfo.InfoHash)
			}
		}()
	}
	wg.Wait()
}

func TestSeederTTI(t *testing.T) {
	require := require.New(t)

	mocks, cleanup := newTestMocks(t)
	defer cleanup()

	config := configFixture()
	config.Conn.DisableThrottling = true

	tf := core.TestTorrentFileFixture()

	mocks.metaInfoClient.EXPECT().Download(
		namespace, tf.MetaInfo.Name()).Return(tf.MetaInfo, nil).Times(2)

	clk := clock.NewMock()
	w := newEventWatcher()

	seeder := mocks.newPeer(config, withEventLoop(w), withClock(clk))
	seeder.writeTorrent(tf)
	require.NoError(<-seeder.scheduler.AddTorrent(namespace, tf.MetaInfo.Name()))

	clk.Add(config.AnnounceInterval)

	leecher := mocks.newPeer(config, withClock(clk))
	errc := leecher.scheduler.AddTorrent(namespace, tf.MetaInfo.Name())

	clk.Add(config.AnnounceInterval)

	require.NoError(<-errc)
	leecher.checkTorrent(t, tf)

	// Conns expire...
	clk.Add(config.ConnTTI)

	clk.Add(config.PreemptionInterval)
	w.WaitFor(t, preemptionTickEvent{})

	// Then seeding torrents expire.
	clk.Add(config.SeederTTI)

	waitForTorrentRemoved(t, seeder.scheduler, tf.MetaInfo.InfoHash)
	waitForTorrentRemoved(t, leecher.scheduler, tf.MetaInfo.InfoHash)

	require.False(hasConn(seeder.scheduler, leecher.pctx.PeerID, tf.MetaInfo.InfoHash))
	require.False(hasConn(leecher.scheduler, seeder.pctx.PeerID, tf.MetaInfo.InfoHash))
}

func TestLeecherTTI(t *testing.T) {
	require := require.New(t)

	mocks, cleanup := newTestMocks(t)
	defer cleanup()

	config := configFixture()
	clk := clock.NewMock()
	w := newEventWatcher()

	tf := core.TestTorrentFileFixture()

	mocks.metaInfoClient.EXPECT().Download(namespace, tf.MetaInfo.Name()).Return(tf.MetaInfo, nil)

	p := mocks.newPeer(config, withEventLoop(w), withClock(clk))
	errc := p.scheduler.AddTorrent(namespace, tf.MetaInfo.Name())

	waitForTorrentAdded(t, p.scheduler, tf.MetaInfo.InfoHash)

	clk.Add(config.LeecherTTI)

	w.WaitFor(t, preemptionTickEvent{})

	require.Equal(ErrTorrentTimeout, <-errc)
}

func TestMultipleAddTorrentsForSameTorrentSucceed(t *testing.T) {
	require := require.New(t)

	mocks, cleanup := newTestMocks(t)
	defer cleanup()

	tf := core.TestTorrentFileFixture()

	// Allow any number of downloads due to concurrency below.
	mocks.metaInfoClient.EXPECT().Download(
		namespace, tf.MetaInfo.Name()).Return(tf.MetaInfo, nil).AnyTimes()

	config := configFixture()

	seeder := mocks.newPeer(config)
	seeder.writeTorrent(tf)
	require.NoError(<-seeder.scheduler.AddTorrent(namespace, tf.MetaInfo.Name()))

	leecher := mocks.newPeer(config)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Multiple goroutines should be able to wait on the same torrent.
			require.NoError(<-leecher.scheduler.AddTorrent(namespace, tf.MetaInfo.Name()))
		}()
	}
	wg.Wait()

	leecher.checkTorrent(t, tf)

	// After the torrent is complete, further calls to AddTorrent should succeed immediately.
	require.NoError(<-leecher.scheduler.AddTorrent(namespace, tf.MetaInfo.Name()))
}

func TestEmitStatsEventTriggers(t *testing.T) {
	mocks, cleanup := newTestMocks(t)
	defer cleanup()

	config := configFixture()
	clk := clock.NewMock()
	w := newEventWatcher()

	mocks.newPeer(config, withEventLoop(w), withClock(clk))

	clk.Add(config.EmitStatsInterval)
	w.WaitFor(t, emitStatsEvent{})
}

func TestNetworkEvents(t *testing.T) {
	t.Skip("Network event timers break this test")

	require := require.New(t)

	mocks, cleanup := newTestMocks(t)
	defer cleanup()

	config := configFixture()
	config.ConnTTI = 2 * time.Second

	seeder := mocks.newPeer(config)
	leecher := mocks.newPeer(config)

	// Torrent with 1 piece.
	tf := core.CustomTestTorrentFileFixture(1, 1)

	mocks.metaInfoClient.EXPECT().Download(
		namespace, tf.MetaInfo.Name()).Return(tf.MetaInfo, nil).Times(2)

	seeder.writeTorrent(tf)
	require.NoError(<-seeder.scheduler.AddTorrent(namespace, tf.MetaInfo.Name()))

	require.NoError(<-leecher.scheduler.AddTorrent(namespace, tf.MetaInfo.Name()))
	leecher.checkTorrent(t, tf)

	sid := seeder.pctx.PeerID
	lid := leecher.pctx.PeerID
	h := tf.MetaInfo.InfoHash

	waitForConnRemoved(t, seeder.scheduler, lid, h)
	waitForConnRemoved(t, leecher.scheduler, sid, h)

	seederExpected := []*networkevent.Event{
		networkevent.AddTorrentEvent(h, sid, storage.BitSetFixture(true), config.ConnState.MaxOpenConnectionsPerTorrent),
		networkevent.TorrentCompleteEvent(h, sid),
		networkevent.AddPendingConnEvent(h, sid, lid),
		networkevent.AddActiveConnEvent(h, sid, lid),
		networkevent.DropActiveConnEvent(h, sid, lid),
		networkevent.BlacklistConnEvent(h, sid, lid, config.ConnState.BlacklistDuration),
	}

	leecherExpected := []*networkevent.Event{
		networkevent.AddTorrentEvent(h, lid, storage.BitSetFixture(false), config.ConnState.MaxOpenConnectionsPerTorrent),
		networkevent.AddPendingConnEvent(h, lid, sid),
		networkevent.AddActiveConnEvent(h, lid, sid),
		networkevent.ReceivePieceEvent(h, lid, sid, 0),
		networkevent.TorrentCompleteEvent(h, lid),
		networkevent.DropActiveConnEvent(h, lid, sid),
		networkevent.BlacklistConnEvent(h, lid, sid, config.ConnState.BlacklistDuration),
	}

	require.Equal(
		networkevent.StripTimestamps(seederExpected),
		networkevent.StripTimestamps(seeder.testProducer.Events()))

	require.Equal(
		networkevent.StripTimestamps(leecherExpected),
		networkevent.StripTimestamps(leecher.testProducer.Events()))
}

func TestPullInactiveTorrent(t *testing.T) {
	require := require.New(t)

	mocks, cleanup := newTestMocks(t)
	defer cleanup()

	config := configFixture()

	tf := core.TestTorrentFileFixture()

	mocks.metaInfoClient.EXPECT().Download(
		namespace, tf.MetaInfo.Name()).Return(tf.MetaInfo, nil).Times(2)

	seeder := mocks.newPeer(config)

	// Write torrent to disk, but don't add it the scheduler.
	seeder.writeTorrent(tf)

	// Force announce the scheduler for this torrent to simulate a peer which
	// is registered in tracker but does not have the torrent in memory.
	ac := announceclient.New(seeder.pctx, serverset.NewSingle(mocks.trackerAddr))
	ac.Announce(tf.MetaInfo.Info.Name, tf.MetaInfo.InfoHash, false)

	leecher := mocks.newPeer(config)

	require.NoError(<-leecher.scheduler.AddTorrent(namespace, tf.MetaInfo.Name()))
	leecher.checkTorrent(t, tf)
}

func TestSchedulerReload(t *testing.T) {
	require := require.New(t)

	mocks, cleanup := newTestMocks(t)
	defer cleanup()

	config := configFixture()

	seeder := mocks.newPeer(config)
	leecher := mocks.newPeer(config)

	download := func() {
		tf := core.TestTorrentFileFixture()

		mocks.metaInfoClient.EXPECT().Download(
			namespace, tf.MetaInfo.Name()).Return(tf.MetaInfo, nil).Times(2)

		seeder.writeTorrent(tf)
		require.NoError(<-seeder.scheduler.AddTorrent(namespace, tf.MetaInfo.Name()))

		require.NoError(<-leecher.scheduler.AddTorrent(namespace, tf.MetaInfo.Name()))
		leecher.checkTorrent(t, tf)
	}

	download()

	config.ConnTTL = 45 * time.Minute
	s, err := Reload(leecher.scheduler, config, tally.NewTestScope("", nil))
	require.NoError(err)
	leecher.scheduler = s

	download()
}

func TestSchedulerRemoveTorrent(t *testing.T) {
	require := require.New(t)

	mocks, cleanup := newTestMocks(t)
	defer cleanup()

	p := mocks.newPeer(configFixture())

	tf := core.TestTorrentFileFixture()

	mocks.metaInfoClient.EXPECT().Download(
		namespace, tf.MetaInfo.Name()).Return(tf.MetaInfo, nil)

	errc := p.scheduler.AddTorrent(namespace, tf.MetaInfo.Name())

	done := make(chan struct{})
	go func() {
		defer close(done)
		require.Equal(ErrTorrentRemoved, <-errc)
	}()

	require.NoError(<-p.scheduler.RemoveTorrent(tf.MetaInfo.Name()))

	<-done

	_, err := p.torrentArchive.Stat(tf.MetaInfo.Name())
	require.True(os.IsNotExist(err))
}

// BENCHMARKS

// NOTE: You'll need to increase your fd limit to around 4096 to run this benchmark.
// You can do this with `ulimit -n 4096`.
func BenchmarkPieceUploadingAndDownloading(b *testing.B) {
	require := require.New(b)

	config := configFixture()
	config.AnnounceInterval = 50 * time.Millisecond

	b.ResetTimer()

	for i := 0; i < b.N; i++ {

		b.StopTimer()

		mocks, cleanup := newTestMocks(b)
		defer func() {
			if err := recover(); err != nil {
				cleanup()
				panic(err)
			}
		}()

		seeder := mocks.newPeer(config)

		var tfs []*core.TestTorrentFile
		for i := 0; i < 10; i++ {
			tf := core.CustomTestTorrentFileFixture(50*memsize.MB, 128*memsize.KB)
			tfs = append(tfs, tf)

			mocks.metaInfoClient.EXPECT().Download(
				namespace, tf.MetaInfo.Name()).Return(tf.MetaInfo, nil).AnyTimes()

			seeder.writeTorrent(tf)
			require.NoError(<-seeder.scheduler.AddTorrent(namespace, tf.MetaInfo.Name()))
		}

		peers := mocks.newPeers(10, config)

		b.StartTimer()
		var wg sync.WaitGroup
		for _, p := range peers {
			for _, tf := range tfs {
				wg.Add(1)
				go func(p *testPeer, tf *core.TestTorrentFile) {
					defer wg.Done()
					require.NoError(<-p.scheduler.AddTorrent(namespace, tf.MetaInfo.Name()))
				}(p, tf)
			}
		}
		wg.Wait()
		b.StopTimer()

		cleanup()
	}
}
