package simple_ondatra_test

import (
	"context"
	"flag"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/open-traffic-generator/snappi/gosnappi"
	"github.com/openconfig/featureprofiles/topologies/binding"
	"github.com/openconfig/ondatra"
	"github.com/openconfig/ondatra/gnmi"
	"github.com/openconfig/ondatra/knebind/solver"
	"github.com/openconfig/ondatra/otg"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	tpb "github.com/openconfig/kne/proto/topo"
	mpb "github.com/openconfig/magna/proto/mirror"
)

const (
	// destinationLabel is the outer label that is used for the generated packets in MPLS flows.
	destinationLabel = 100
	// innerLabel is the inner label used for the generated packets in MPLS flows.
	innerLabel = 5000
)

var (
	// sleepTime allows a user to specify that the test should sleep after setting
	// up all elements (configuration, gRIBI forwarding entries, traffic flows etc.).
	sleepTime = flag.Duration("sleep", 10*time.Second, "duration for which the test should sleep after setup")
)

// intf is a simple description of an interface.
type intf struct {
	// Name is the name of the interface.
	Name string
	// MAC is the MAC address for the interface.
	MAC string
}

var (
	// ateSrc describes the configuration parameters for the ATE port sourcing
	// a flow.
	ateSrc = &intf{
		Name: "port1",
		MAC:  "02:00:01:01:01:01",
	}

	ateDst = &intf{
		Name: "port2",
		MAC:  "02:00:02:01:01:01",
	}

	ateAuxDst = &intf{
		Name: "port3",
		MAC:  "03:00:03:01:01:01",
	}
)

func TestMain(m *testing.M) {
	ondatra.RunTests(m, binding.New)
}

// configureATE interfaces configrues the source and destination ports on ATE according to the specifications
// above. It returns the OTG configuration object.
func configureATEInterfaces(t *testing.T, ate *ondatra.ATEDevice, srcATE, dstATE *intf) (gosnappi.Config, error) {
	otg := ate.OTG()
	topology := otg.NewConfig(t)
	for _, p := range []*intf{ateSrc, ateDst, ateAuxDst} {
		topology.Ports().Add().SetName(p.Name)
		dev := topology.Devices().Add().SetName(p.Name)
		eth := dev.Ethernets().Add().SetName(fmt.Sprintf("%s_ETH", p.Name)).SetMac(p.MAC)
		eth.Connection().SetPortName(dev.Name())
	}

	c, err := topology.ToJson()
	if err != nil {
		return topology, err
	}
	t.Logf("configuration for OTG is %s", c)

	otg.PushConfig(t, topology)
	return topology, nil
}

// pushBaseConfigs pushes the base configuration to the ATE device in the test
// topology.
func pushBaseConfigs(t *testing.T, ate *ondatra.ATEDevice) gosnappi.Config {
	otgCfg, err := configureATEInterfaces(t, ate, ateSrc, ateDst)
	if err != nil {
		t.Fatalf("cannot configure ATE interfaces via OTG, %v", err)
	}

	return otgCfg
}

// mirrorAddr retrieves the address of the mirror container in the topology.
func mirrorAddr(t *testing.T) string {
	t.Helper()
	mirror := ondatra.DUT(t, "mirror")
	data := mirror.CustomData(solver.KNEServiceMapKey).(map[string]*tpb.Service)
	m := data["mirror-controller"]
	if m == nil {
		t.Fatalf("cannot find mirror data, got: %v", data)
	}
	return net.JoinHostPort(m.GetOutsideIp(), strconv.Itoa(int(m.GetOutside())))
}

// mirrorClient creates a new gRPC client for the mirror service.
func mirrorClient(t *testing.T, addr string) (mpb.MirrorClient, func() error) {
	t.Helper()
	conn, err := grpc.Dial(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		t.Fatalf("cannot dial mirror, got err: %v", err)
	}

	return mpb.NewMirrorClient(conn), conn.Close
}

// startTwoPortMirror begins traffic mirroring between port1 and port2 on the mirror
// container in the topology.
func startTwoPortMirror(t *testing.T, client mpb.MirrorClient) {
	t.Helper()
	mirror := ondatra.DUT(t, "mirror")
	startMirrors(t, client, &mpb.StartRequest{
		From: mirror.Port(t, "port1").Name(),
		To:   mirror.Port(t, "port2").Name(),
	})
}

// startMirrors starts the mirrors described by the supplied requests.
func startMirrors(t *testing.T, client mpb.MirrorClient, reqs ...*mpb.StartRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, sr := range reqs {
		if _, err := client.Start(ctx, sr); err != nil {
			t.Fatalf("cannot start mirror client, got err: %v", err)
		}
	}
}

// stopTwoPortMirror stops traffic mirroring between port1 and port2 on the mirror
// container in the topology.
func stopTwoPortMirror(t *testing.T, client mpb.MirrorClient) {
	t.Helper()
	mirror := ondatra.DUT(t, "mirror")
	stopMirrors(t, client, &mpb.StopRequest{
		From: mirror.Port(t, "port1").Name(),
		To:   mirror.Port(t, "port2").Name(),
	})

}

// stopMirrors stops the mirrors described by the supplied requests.
func stopMirrors(t *testing.T, client mpb.MirrorClient, reqs ...*mpb.StopRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, sr := range reqs {
		if _, err := client.Stop(ctx, sr); err != nil {
			t.Fatalf("cannot stop mirror client, got err: %v", err)
		}
	}
}

// TestMirror is a simple test that validates that the mirror service can
// be contacted and the RPCs to start and stop traffic mirroring return
// successful responses. It does not validate that the traffic mirroring
// happens successfully.
func TestMirror(t *testing.T) {
	addr := mirrorAddr(t)
	client, stop := mirrorClient(t, addr)
	defer stop()
	startTwoPortMirror(t, client)
	time.Sleep(1 * time.Second)
	stopTwoPortMirror(t, client)
}

// addMPLSFlow adds a new MPLS flow to the specified otgCfg. The flow is named
// according to the name argument, and runs between a srcName and dstName port,
// with the srcv4 IPv4 source address, and dstv4 destination address. The flow
// runs at pps packets per second, until totPackets packets have been sent. If
// totPackets is 0, packets are sent at pps until the flow is terminated
// through OTG.
func addMPLSFlow(t *testing.T, otgCfg gosnappi.Config, name, srcName, dstName, srcv4, dstv4 string, pps uint64, totPackets uint32) {
	mplsFlow := otgCfg.Flows().Add().SetName(name)
	mplsFlow.Metrics().SetEnable(true)
	mplsFlow.TxRx().Port().SetTxName(srcName).SetRxNames([]string{dstName})

	mplsFlow.Rate().SetChoice("pps").SetPps(pps)
	if totPackets != 0 {
		mplsFlow.Duration().SetChoice("fixed_packets").FixedPackets().SetPackets(totPackets)
	}

	// OTG specifies that the order of the <flow>.Packet().Add() calls determines
	// the stack of headers that are to be used, starting at the outer-most and
	// ending with the inner-most.

	// Set up ethernet layer.
	eth := mplsFlow.Packet().Add().Ethernet()
	eth.Src().SetChoice("value").SetValue(ateSrc.MAC)
	eth.Dst().SetChoice("value").SetValue(ateDst.MAC)

	// Set up MPLS layer with destination label.
	mpls := mplsFlow.Packet().Add().Mpls()
	mpls.Label().SetChoice("value").SetValue(destinationLabel)
	mpls.BottomOfStack().SetChoice("value").SetValue(0)

	mplsInner := mplsFlow.Packet().Add().Mpls()
	mplsInner.Label().SetChoice("value").SetValue(innerLabel)
	mplsInner.BottomOfStack().SetChoice("value").SetValue(1)

	ip4 := mplsFlow.Packet().Add().Ipv4()
	ip4.Src().SetChoice("value").SetValue(srcv4)
	ip4.Dst().SetChoice("value").SetValue(dstv4)
	ip4.Version().SetChoice("value").SetValue(4)

}

const (
	// lossTolerance indicates the number of packets we are prepared to lose during
	// a test. If the packets per second generation rate is low then the flow can be
	// stopped before a slow packet generator creates the next packet.
	lossTolerance = 1
)

// TestBasicMPLS is a simple test that creates an MPLS flow between two ATE ports and
// checks that there is no packet loss. It validates magna's end-to-end MPLS
// flow accounting.
func TestBasicMPLS(t *testing.T) {
	// Start a mirroring session to copy packets.
	maddr := mirrorAddr(t)
	client, stop := mirrorClient(t, maddr)
	defer stop()
	startTwoPortMirror(t, client)
	time.Sleep(1 * time.Second)
	defer func() { stopTwoPortMirror(t, client) }()

	otgCfg := pushBaseConfigs(t, ondatra.ATE(t, "ate"))

	otg := ondatra.ATE(t, "ate").OTG()
	otgCfg.Flows().Clear().Items()
	packets := uint32((*sleepTime).Seconds() * 0.9)
	t.Logf("sending %d packets in %s", packets, *sleepTime)
	addMPLSFlow(t, otgCfg, "MPLS_FLOW", ateSrc.Name, ateDst.Name, "100.64.1.1", "100.64.1.2", 1, packets)

	otg.PushConfig(t, otgCfg)

	t.Logf("Starting MPLS traffic...")
	otg.StartTraffic(t)
	t.Logf("Sleeping for %s...", *sleepTime)
	time.Sleep(*sleepTime)
	t.Logf("Stopping MPLS traffic...")
	otg.StopTraffic(t)

	// Avoid a race with telemetry updates.
	time.Sleep(10 * time.Second)
	metrics := gnmi.Get(t, otg, gnmi.OTG().Flow("MPLS_FLOW").State())
	got, want := metrics.GetCounters().GetInPkts(), metrics.GetCounters().GetOutPkts()
	if lossPackets := want - got; lossPackets > lossTolerance {
		t.Fatalf("did not get expected number of packets, got: %d, want: %d", got, want)
	}
}

// checkFlow validates that OTG flow name passes the specified check function.
func checkFlow(t *testing.T, otg *otg.OTG, name string, testFn func(got, want uint64) error) {
	t.Helper()
	metrics := gnmi.Get(t, otg, gnmi.OTG().Flow(name).State())
	got, want := metrics.GetCounters().GetInPkts(), metrics.GetCounters().GetOutPkts()
	t.Logf("%s: recv: %d, sent: %d packets", name, got, want)
	if err := testFn(got, want); err != nil {
		t.Fatalf("%s: did not get expected number of packets, %v", name, err)
	}
}

// toleranceFn is a check function that ensures that the packets lost on the specified flow
// are less than the lossTolerance constant.
func toleranceFn(got, want uint64) error {
	if lossPackets := want - got; lossPackets > lossTolerance {
		return fmt.Errorf("got: %d, want: >= %d-%d", got, want, lossTolerance)
	}
	return nil
}

// TestMPLSFlowsTwoPorts validates that multiple MPLS flows work with magna.
func TestMPLSFlowsTwoPorts(t *testing.T) {
	tests := []struct {
		desc      string
		inFlowFn  func(*testing.T, gosnappi.Config)
		inCheckFn func(*testing.T, *otg.OTG)
	}{{
		desc: "two flows - same source port",
		inFlowFn: func(t *testing.T, cfg gosnappi.Config) {
			addMPLSFlow(t, cfg, "FLOW_ONE", ateSrc.Name, ateDst.Name, "100.64.1.1", "100.64.1.2", 1, 0)
			addMPLSFlow(t, cfg, "FLOW_TWO", ateSrc.Name, ateDst.Name, "100.64.2.1", "100.64.2.2", 1, 0)
		},
		inCheckFn: func(t *testing.T, otgc *otg.OTG) {
			for _, f := range []string{"FLOW_ONE", "FLOW_TWO"} {
				checkFlow(t, otgc, f, toleranceFn)
			}
		},
	}, {
		desc: "failure - two flows, one that is not mirrored",
		inFlowFn: func(t *testing.T, cfg gosnappi.Config) {
			addMPLSFlow(t, cfg, "A->B", ateSrc.Name, ateDst.Name, "100.64.1.1", "100.64.1.2", 1, 0)
			addMPLSFlow(t, cfg, "B->A", ateDst.Name, ateSrc.Name, "100.64.2.1", "100.64.2.2", 2, 0)
		},
		inCheckFn: func(t *testing.T, otgc *otg.OTG) {
			checkFlow(t, otgc, "A->B", toleranceFn)
			checkFlow(t, otgc, "B->A", func(got, want uint64) error {
				if got != 0 {
					return fmt.Errorf("got: %d packets, want: 0", got)
				}
				return nil
			})
		},
	}, {
		desc: "ten flows",
		inFlowFn: func(t *testing.T, cfg gosnappi.Config) {
			for i := 0; i < 10; i++ {
				addMPLSFlow(t, cfg, fmt.Sprintf("flow%d", i), ateSrc.Name, ateDst.Name, fmt.Sprintf("100.64.%d.1", i), fmt.Sprintf("100.64.%d.2", i), 1, 0)
			}
		},
		inCheckFn: func(t *testing.T, otgc *otg.OTG) {
			for i := 0; i < 10; i++ {
				checkFlow(t, otgc, fmt.Sprintf("flow%d", i), toleranceFn)
			}
		},
	}}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			maddr := mirrorAddr(t)
			client, stop := mirrorClient(t, maddr)
			defer stop()
			startTwoPortMirror(t, client)
			time.Sleep(1 * time.Second)
			defer func() { stopTwoPortMirror(t, client) }()

			otgCfg := pushBaseConfigs(t, ondatra.ATE(t, "ate"))

			otg := ondatra.ATE(t, "ate").OTG()
			otgCfg.Flows().Clear().Items()

			tt.inFlowFn(t, otgCfg)

			otg.PushConfig(t, otgCfg)

			t.Logf("Starting MPLS traffic...")
			otg.StartTraffic(t)
			t.Logf("Sleeping for %s...", *sleepTime)
			time.Sleep(*sleepTime)
			t.Logf("Stopping MPLS traffic...")
			otg.StopTraffic(t)

			time.Sleep(1 * time.Second)

			tt.inCheckFn(t, otg)

		})
	}
}

// startThreePortMirror starts two mirroring sessions:
//
//   - the first copies traffic from port1 -> port2, so any packet received on port1 is received by the device connected to port2.
//   - the second copies traffic from port3 -> port2, so any packet received on port3 is received by the device connected to port2.
//
// This results in port2 receiving all the packets that are transmitted by magna on ports 1+3.
func startThreePortMirror(t *testing.T, client mpb.MirrorClient) {
	t.Helper()
	mirror := ondatra.DUT(t, "mirror")
	startMirrors(t, client,
		&mpb.StartRequest{
			From: mirror.Port(t, "port1").Name(),
			To:   mirror.Port(t, "port2").Name(),
		},
		&mpb.StartRequest{
			From: mirror.Port(t, "port3").Name(),
			To:   mirror.Port(t, "port2").Name(),
		},
	)
}

// stopThreePortMirror stops the mirroring of traffic between port1->port2 and port3->port2.
func stopThreePortMirror(t *testing.T, client mpb.MirrorClient) {
	t.Helper()
	mirror := ondatra.DUT(t, "mirror")
	stopMirrors(t, client,
		&mpb.StopRequest{
			From: mirror.Port(t, "port1").Name(),
			To:   mirror.Port(t, "port2").Name(),
		},
		&mpb.StopRequest{
			From: mirror.Port(t, "port3").Name(),
			To:   mirror.Port(t, "port2").Name(),
		},
	)
}

func TestMPLSFlowsThreePorts(t *testing.T) {
	tests := []struct {
		desc      string
		inFlowFn  func(*testing.T, gosnappi.Config)
		inCheckFn func(*testing.T, *otg.OTG)
	}{{
		desc: "one flow each",
		inFlowFn: func(t *testing.T, cfg gosnappi.Config) {
			addMPLSFlow(t, cfg, "port1->port2", "port1", "port2", "100.64.1.1", "100.64.1.2", 1, 0)
			addMPLSFlow(t, cfg, "port3->port2", "port3", "port2", "100.64.2.1", "100.64.2.2", 1, 0)
		},
		inCheckFn: func(t *testing.T, otgc *otg.OTG) {
			for _, f := range []string{"port1->port2", "port3->port2"} {
				checkFlow(t, otgc, f, toleranceFn)
			}
		},
	}, {
		desc: "ten flows on each port",
		inFlowFn: func(t *testing.T, cfg gosnappi.Config) {
			for i := 0; i < 10; i++ {
				addMPLSFlow(t, cfg, fmt.Sprintf("port1->port2_%d", i), "port1", "port2", fmt.Sprintf("100.64.%d.1", i), fmt.Sprintf("100.64.%d.2", i), 1, 0)
				addMPLSFlow(t, cfg, fmt.Sprintf("port3->port2_%d", i), "port3", "port2", fmt.Sprintf("100.64.%d.1", i+20), fmt.Sprintf("100.64.%d.2", i+20), 1, 0)
			}
		},
		inCheckFn: func(t *testing.T, otgc *otg.OTG) {
			for i := 0; i < 10; i++ {
				for _, f := range []string{"port1->port2", "port3->port2"} {
					checkFlow(t, otgc, fmt.Sprintf("%s_%d", f, i), toleranceFn)
				}
			}
		},
	}}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			maddr := mirrorAddr(t)
			client, stop := mirrorClient(t, maddr)
			defer stop()
			startThreePortMirror(t, client)
			time.Sleep(1 * time.Second)
			defer func() { stopThreePortMirror(t, client) }()

			otgCfg := pushBaseConfigs(t, ondatra.ATE(t, "ate"))

			otg := ondatra.ATE(t, "ate").OTG()
			otgCfg.Flows().Clear().Items()

			tt.inFlowFn(t, otgCfg)

			otg.PushConfig(t, otgCfg)

			t.Logf("Starting MPLS traffic...")
			otg.StartTraffic(t)
			t.Logf("Sleeping for %s...", *sleepTime)
			time.Sleep(*sleepTime)
			t.Logf("Stopping MPLS traffic...")
			otg.StopTraffic(t)

			time.Sleep(10 * time.Second)

			tt.inCheckFn(t, otg)

		})
	}
}
