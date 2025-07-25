package referenceframe

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"

	"github.com/golang/geo/r3"
	"github.com/pkg/errors"
	"go.uber.org/multierr"
	commonpb "go.viam.com/api/common/v1"
	pb "go.viam.com/api/component/arm/v1"

	"go.viam.com/rdk/spatialmath"
)

// A Model represents a frame that can change its name, and can return itself as a ModelConfig struct.
type Model interface {
	Frame
	ModelConfig() *ModelConfigJSON
	ModelPieceFrames([]Input) (map[string]Frame, error)
}

// KinematicModelFromProtobuf returns a model from a protobuf message representing it.
func KinematicModelFromProtobuf(name string, resp *commonpb.GetKinematicsResponse) (Model, error) {
	if resp == nil {
		return nil, errors.New("*commonpb.GetKinematicsResponse can't be nil")
	}
	format := resp.GetFormat()
	data := resp.GetKinematicsData()

	switch format {
	case commonpb.KinematicsFileFormat_KINEMATICS_FILE_FORMAT_SVA:
		return UnmarshalModelJSON(data, name)
	case commonpb.KinematicsFileFormat_KINEMATICS_FILE_FORMAT_URDF:
		modelconf, err := UnmarshalModelXML(data, name)
		if err != nil {
			return nil, err
		}
		return modelconf.ParseConfig(name)
	case commonpb.KinematicsFileFormat_KINEMATICS_FILE_FORMAT_UNSPECIFIED:
		fallthrough
	default:
		if formatName, ok := commonpb.KinematicsFileFormat_name[int32(format)]; ok {
			return nil, fmt.Errorf("unable to parse file of type %s", formatName)
		}
		return nil, fmt.Errorf("unable to parse unknown file type %d", format)
	}
}

// KinematicModelToProtobuf converts a model into a protobuf message version of that model.
func KinematicModelToProtobuf(model Model) *commonpb.GetKinematicsResponse {
	if model == nil {
		return &commonpb.GetKinematicsResponse{Format: commonpb.KinematicsFileFormat_KINEMATICS_FILE_FORMAT_UNSPECIFIED}
	}

	cfg := model.ModelConfig()
	if cfg == nil || cfg.OriginalFile == nil {
		return &commonpb.GetKinematicsResponse{Format: commonpb.KinematicsFileFormat_KINEMATICS_FILE_FORMAT_UNSPECIFIED}
	}
	resp := &commonpb.GetKinematicsResponse{KinematicsData: cfg.OriginalFile.Bytes}
	switch cfg.OriginalFile.Extension {
	case "json":
		resp.Format = commonpb.KinematicsFileFormat_KINEMATICS_FILE_FORMAT_SVA
	case "urdf":
		resp.Format = commonpb.KinematicsFileFormat_KINEMATICS_FILE_FORMAT_URDF
	default:
		resp.Format = commonpb.KinematicsFileFormat_KINEMATICS_FILE_FORMAT_UNSPECIFIED
	}
	return resp
}

// KinematicModelFromFile returns a model frame from a file that defines the kinematics.
func KinematicModelFromFile(modelPath, name string) (Model, error) {
	switch {
	case strings.HasSuffix(modelPath, ".urdf"):
		return ParseModelXMLFile(modelPath, name)
	case strings.HasSuffix(modelPath, ".json"):
		return ParseModelJSONFile(modelPath, name)
	default:
		return nil, errors.New("only files with .json and .urdf file extensions are supported")
	}
}

// SimpleModel is a model that serially concatenates a list of Frames.
type SimpleModel struct {
	*baseFrame
	// OrdTransforms is the list of transforms ordered from end effector to base
	OrdTransforms []Frame
	modelConfig   *ModelConfigJSON
	poseCache     sync.Map
	lock          sync.RWMutex
}

// NewSimpleModel constructs a new model.
func NewSimpleModel(name string) *SimpleModel {
	return &SimpleModel{
		baseFrame: &baseFrame{name: name},
	}
}

// GenerateRandomConfiguration generates a list of radian joint positions that are random but valid for each joint.
func GenerateRandomConfiguration(m Model, randSeed *rand.Rand) []float64 {
	limits := m.DoF()
	jointPos := make([]float64, 0, len(limits))

	for i := 0; i < len(limits); i++ {
		jRange := math.Abs(limits[i].Max - limits[i].Min)
		// Note that rand is unseeded and so will produce the same sequence of floats every time
		// However, since this will presumably happen at different positions to different joints, this shouldn't matter
		newPos := randSeed.Float64()*jRange + limits[i].Min
		jointPos = append(jointPos, newPos)
	}
	return jointPos
}

// ModelConfig returns the ModelConfig object used to create this model.
func (m *SimpleModel) ModelConfig() *ModelConfigJSON {
	return m.modelConfig
}

// Transform takes a model and a list of joint angles in radians and computes the dual quaternion representing the
// cartesian position of the end effector. This is useful for when conversions between quaternions and OV are not needed.
func (m *SimpleModel) Transform(inputs []Input) (spatialmath.Pose, error) {
	frames, err := m.inputsToFrames(inputs, false)
	if err != nil && frames == nil {
		return nil, err
	}
	return frames[0].transform, err
}

// Interpolate interpolates the given amount between the two sets of inputs.
func (m *SimpleModel) Interpolate(from, to []Input, by float64) ([]Input, error) {
	interp := make([]Input, 0, len(from))
	posIdx := 0
	for _, transform := range m.OrdTransforms {
		dof := len(transform.DoF()) + posIdx
		fromSubset := from[posIdx:dof]
		toSubset := to[posIdx:dof]
		posIdx = dof

		interpSubset, err := transform.Interpolate(fromSubset, toSubset, by)
		if err != nil {
			return nil, err
		}
		interp = append(interp, interpSubset...)
	}
	return interp, nil
}

// InputFromProtobuf converts pb.JointPosition to inputs.
func (m *SimpleModel) InputFromProtobuf(jp *pb.JointPositions) []Input {
	inputs := make([]Input, 0, len(jp.Values))
	posIdx := 0
	for _, transform := range m.OrdTransforms {
		dof := len(transform.DoF()) + posIdx
		jPos := jp.Values[posIdx:dof]
		posIdx = dof

		inputs = append(inputs, transform.InputFromProtobuf(&pb.JointPositions{Values: jPos})...)
	}

	return inputs
}

// ProtobufFromInput converts inputs to pb.JointPosition.
func (m *SimpleModel) ProtobufFromInput(input []Input) *pb.JointPositions {
	jPos := &pb.JointPositions{}
	posIdx := 0
	for _, transform := range m.OrdTransforms {
		dof := len(transform.DoF()) + posIdx
		jPos.Values = append(jPos.Values, transform.ProtobufFromInput(input[posIdx:dof]).Values...)
		posIdx = dof
	}

	return jPos
}

// Geometries returns an object representing the 3D space associeted with the staticFrame.
func (m *SimpleModel) Geometries(inputs []Input) (*GeometriesInFrame, error) {
	frames, err := m.inputsToFrames(inputs, true)
	if err != nil && frames == nil {
		return nil, err
	}
	var errAll error
	geometries := make([]spatialmath.Geometry, 0, len(frames))
	for _, frame := range frames {
		geometriesInFrame, err := frame.Geometries([]Input{})
		if err != nil {
			multierr.AppendInto(&errAll, err)
			continue
		}
		for _, geom := range geometriesInFrame.Geometries() {
			placedGeom := geom.Transform(frame.transform)
			placedGeom.SetLabel(m.name + ":" + geom.Label())
			geometries = append(geometries, placedGeom)
		}
	}
	return NewGeometriesInFrame(m.name, geometries), errAll
}

// CachedTransform will check a sync.Map cache to see if the exact given set of inputs has been computed yet. If so
// it returns without redoing the calculation. Thread safe, but so far has tended to be slightly slower than just doing
// the calculation. This may change with higher DOF models and longer runtimes.
func (m *SimpleModel) CachedTransform(inputs []Input) (spatialmath.Pose, error) {
	key := floatsToString(inputs)
	if val, ok := m.poseCache.Load(key); ok {
		if pose, ok := val.(spatialmath.Pose); ok {
			return pose, nil
		}
	}
	poses, err := m.inputsToFrames(inputs, false)
	if err != nil && poses == nil {
		return nil, err
	}
	m.poseCache.Store(key, poses[len(poses)-1].transform)

	return poses[len(poses)-1].transform, err
}

// DoF returns the number of degrees of freedom within a model.
func (m *SimpleModel) DoF() []Limit {
	m.lock.RLock()
	if len(m.limits) > 0 {
		defer m.lock.RUnlock()
		return m.limits
	}
	m.lock.RUnlock()

	limits := make([]Limit, 0, len(m.OrdTransforms))
	for _, transform := range m.OrdTransforms {
		if len(transform.DoF()) > 0 {
			limits = append(limits, transform.DoF()...)
		}
	}
	m.lock.Lock()
	m.limits = limits
	m.lock.Unlock()
	return limits
}

// MarshalJSON serializes a Model.
func (m *SimpleModel) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.modelConfig)
}

// UnmarshalJSON deserializes a Model.
func (m *SimpleModel) UnmarshalJSON(data []byte) error {
	var modelConfig ModelConfigJSON
	if err := json.Unmarshal(data, &modelConfig); err != nil {
		return err
	}
	m.baseFrame = &baseFrame{name: modelConfig.Name}
	m.modelConfig = &modelConfig
	return nil
}

// ModelPieceFrames takes a list of inputs and returns a map of frame names to their corresponding static frames,
// effectively breaking the model into its kinematic pieces.
func (m *SimpleModel) ModelPieceFrames(inputs []Input) (map[string]Frame, error) {
	poses, err := m.inputsToFrames(inputs, true)
	if err != nil {
		return nil, err
	}
	frameMap := map[string]Frame{}
	for _, sFrame := range poses {
		frameMap[sFrame.Name()] = sFrame
	}
	return frameMap, nil
}

// inputsToFrames takes a model and a list of joint angles in radians and computes the dual quaternion representing the
// cartesian position of each of the links up to and including the end effector. This is useful for when conversions
// between quaternions and OV are not needed.
func (m *SimpleModel) inputsToFrames(inputs []Input, collectAll bool) ([]*staticFrame, error) {
	if len(m.DoF()) != len(inputs) {
		return nil, NewIncorrectDoFError(len(inputs), len(m.DoF()))
	}
	var err error
	poses := make([]*staticFrame, 0, len(m.OrdTransforms))
	// Start at ((1+0i+0j+0k)+(+0+0i+0j+0k)ϵ)
	composedTransformation := spatialmath.NewZeroPose()
	posIdx := 0
	// get quaternions from the base outwards.
	for _, transform := range m.OrdTransforms {
		dof := len(transform.DoF()) + posIdx
		input := inputs[posIdx:dof]
		posIdx = dof

		pose, errNew := transform.Transform(input)
		// Fail if inputs are incorrect and pose is nil, but allow querying out-of-bounds positions
		if pose == nil || (err != nil && !strings.Contains(err.Error(), OOBErrString)) {
			return nil, err
		}
		multierr.AppendInto(&err, errNew)
		if collectAll {
			var geometry spatialmath.Geometry
			gf, err := transform.Geometries(input)
			if err != nil {
				return nil, err
			}
			geometries := gf.Geometries()
			if len(geometries) == 0 {
				geometry = nil
			} else {
				geometry = geometries[0]
			}
			// TODO(pl): Part of the implementation for GetGeometries will require removing the single geometry restriction
			fixedFrame, err := NewStaticFrameWithGeometry(transform.Name(), composedTransformation, geometry)
			if err != nil {
				return nil, err
			}
			poses = append(poses, fixedFrame.(*staticFrame))
		}
		composedTransformation = spatialmath.Compose(composedTransformation, pose)
	}
	// TODO(rb) as written this will return one too many frames, no need to return zeroth frame
	poses = append(poses, &staticFrame{&baseFrame{"", []Limit{}}, composedTransformation, nil})
	return poses, err
}

// floatsToString turns a float array into a serializable binary representation
// This is very fast, about 100ns per call.
func floatsToString(inputs []Input) string {
	b := make([]byte, len(inputs)*8)
	for i, input := range inputs {
		binary.BigEndian.PutUint64(b[8*i:8*i+8], math.Float64bits(input.Value))
	}
	return string(b)
}

// New2DMobileModelFrame builds the kinematic model associated with the kinematicWheeledBase
// This model is intended to be used with a mobile base and has either 2DOF corresponding to  a state of x, y
// or has 3DOF corresponding to a state of x, y, and theta, where x and y are the positional coordinates
// the base is located about and theta is the rotation about the z axis.
func New2DMobileModelFrame(name string, limits []Limit, collisionGeometry spatialmath.Geometry) (Model, error) {
	if len(limits) != 2 && len(limits) != 3 {
		return nil,
			errors.Errorf("Must have 2DOF state (x, y) or 3DOF state (x, y, theta) to create 2DMobileModelFrame, have %d dof", len(limits))
	}

	// build the model - SLAM convention is that the XY plane is the ground plane
	x, err := NewTranslationalFrame("x", r3.Vector{X: 1}, limits[0])
	if err != nil {
		return nil, err
	}
	y, err := NewTranslationalFrame("y", r3.Vector{Y: 1}, limits[1])
	if err != nil {
		return nil, err
	}
	geometry, err := NewStaticFrameWithGeometry("geometry", spatialmath.NewZeroPose(), collisionGeometry)
	if err != nil {
		return nil, err
	}

	model := NewSimpleModel(name)
	if len(limits) == 3 {
		theta, err := NewRotationalFrame("theta", *spatialmath.NewR4AA(), limits[2])
		if err != nil {
			return nil, err
		}
		model.OrdTransforms = []Frame{x, y, theta, geometry}
	} else {
		model.OrdTransforms = []Frame{x, y, geometry}
	}
	return model, nil
}

// ComputeOOBPosition takes a frame and a slice of Inputs and returns the cartesian position of the frame after
// transforming it by the given inputs even when if the inputs given would violate the Limits of the frame.
// This is performed statelessly without changing any data.
func ComputeOOBPosition(frame Frame, inputs []Input) (spatialmath.Pose, error) {
	if inputs == nil {
		return nil, errors.New("cannot compute position for nil joints")
	}
	if frame == nil {
		return nil, errors.New("cannot compute position for nil frame")
	}

	pose, err := frame.Transform(inputs)
	if err != nil && !strings.Contains(err.Error(), OOBErrString) {
		return nil, err
	}

	return pose, nil
}
