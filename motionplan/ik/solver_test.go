package ik

import (
	"context"
	"errors"
	"math"
	"runtime"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/test"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/motionplan"
	frame "go.viam.com/rdk/referenceframe"
	spatial "go.viam.com/rdk/spatialmath"
	"go.viam.com/rdk/utils"
)

var (
	home = []float64{0, 0, 0, 0, 0, 0}
	nCPU = int(math.Max(1.0, float64(runtime.NumCPU()/4)))
)

func TestCombinedIKinematics(t *testing.T) {
	logger := logging.NewTestLogger(t)
	m, err := frame.ParseModelJSONFile(utils.ResolveFile("components/arm/example_kinematics/xarm6_kinematics_test.json"), "")
	test.That(t, err, test.ShouldBeNil)
	ik, err := CreateCombinedIKSolver(m.DoF(), logger, nCPU, defaultGoalThreshold)
	test.That(t, err, test.ShouldBeNil)

	// Test ability to arrive at another position
	pos := spatial.NewPose(
		r3.Vector{X: -46, Y: -133, Z: 372},
		&spatial.OrientationVectorDegrees{OX: 1.79, OY: -1.32, OZ: -1.11},
	)
	solveFunc := NewMetricMinFunc(motionplan.NewSquaredNormMetric(pos), m, logger)
	solution, err := solveTest(context.Background(), ik, solveFunc, home)
	test.That(t, err, test.ShouldBeNil)

	// Test moving forward 20 in X direction from previous position
	pos = spatial.NewPose(
		r3.Vector{X: -66, Y: -133, Z: 372},
		&spatial.OrientationVectorDegrees{OX: 1.78, OY: -3.3, OZ: -1.11},
	)
	solveFunc = NewMetricMinFunc(motionplan.NewSquaredNormMetric(pos), m, logger)
	_, err = solveTest(context.Background(), ik, solveFunc, solution[0])
	test.That(t, err, test.ShouldBeNil)
}

func TestUR5NloptIKinematics(t *testing.T) {
	logger := logging.NewTestLogger(t)

	m, err := frame.ParseModelJSONFile(utils.ResolveFile("components/arm/example_kinematics/ur5e.json"), "")
	test.That(t, err, test.ShouldBeNil)
	ik, err := CreateCombinedIKSolver(m.DoF(), logger, nCPU, defaultGoalThreshold)
	test.That(t, err, test.ShouldBeNil)

	goalJP := frame.JointPositionsFromRadians([]float64{-4.128, 2.71, 2.798, 2.3, 1.291, 0.62})
	goal, err := m.Transform(m.InputFromProtobuf(goalJP))
	test.That(t, err, test.ShouldBeNil)
	solveFunc := NewMetricMinFunc(motionplan.NewSquaredNormMetric(goal), m, logger)
	_, err = solveTest(context.Background(), ik, solveFunc, home)
	test.That(t, err, test.ShouldBeNil)
}

func TestCombinedCPUs(t *testing.T) {
	logger := logging.NewTestLogger(t)
	m, err := frame.ParseModelJSONFile(utils.ResolveFile("components/arm/example_kinematics/xarm7_kinematics_test.json"), "")
	test.That(t, err, test.ShouldBeNil)
	ik, err := CreateCombinedIKSolver(m.DoF(), logger, runtime.NumCPU()/400000, defaultGoalThreshold)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(ik.(*combinedIK).solvers), test.ShouldEqual, 1)
}

func solveTest(ctx context.Context, solver Solver, solveFunc func([]float64) float64, seed []float64) ([][]float64, error) {
	solutionGen := make(chan *Solution)
	ikErr := make(chan error)
	ctxWithCancel, cancel := context.WithCancel(ctx)
	defer cancel()

	// Spawn the IK solver to generate solutions until done
	go func() {
		defer close(ikErr)
		ikErr <- solver.Solve(ctxWithCancel, solutionGen, seed, solveFunc, 1)
	}()

	var solutions [][]float64

	// Solve the IK solver. Loop labels are required because `break` etc in a `select` will break only the `select`.
IK:
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		select {
		case step := <-solutionGen:
			solutions = append(solutions, step.Configuration)
			// Skip the return check below until we have nothing left to read from solutionGen
			continue IK
		default:
		}

		select {
		case <-ikErr:
			// If we have a return from the IK solver, there are no more solutions, so we finish processing above
			// until we've drained the channel
			break IK
		default:
		}
	}
	cancel()
	if len(solutions) == 0 {
		return nil, errors.New("unable to solve for position")
	}

	return solutions, nil
}
