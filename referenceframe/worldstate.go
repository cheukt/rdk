package referenceframe

import (
	"fmt"
	"strconv"

	"github.com/jedib0t/go-pretty/v6/table"
	commonpb "go.viam.com/api/common/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"go.viam.com/rdk/spatialmath"
)

const unnamedWorldStateGeometryPrefix = "unnamedWorldStateGeometry_"

// WorldState is a struct to store the data representation of the robot's environment.
type WorldState struct {
	obstacleNames map[string]bool
	obstacles     []*GeometriesInFrame
	transforms    []*LinkInFrame
}

// NewEmptyWorldState is a constructor for a WorldState object that has no obstacles or transforms.
func NewEmptyWorldState() *WorldState {
	return &WorldState{
		obstacleNames: make(map[string]bool),
		obstacles:     make([]*GeometriesInFrame, 0),
		transforms:    make([]*LinkInFrame, 0),
	}
}

// NewWorldState instantiates a WorldState with geometries which are meant to represent obstacles
// and transforms which are meant to represent additional links that augment a FrameSystem.
func NewWorldState(obstacles []*GeometriesInFrame, transforms []*LinkInFrame) (*WorldState, error) {
	ws := &WorldState{
		obstacleNames: make(map[string]bool),
		obstacles:     make([]*GeometriesInFrame, 0),
		transforms:    transforms,
	}
	unnamedCount := 0
	for _, gf := range obstacles {
		geometries := gf.Geometries()
		checkedGeometries := make([]spatialmath.Geometry, 0, len(geometries))

		// iterate over geometries and make sure that each one that is added to the WorldState has a unique name
		for _, geometry := range geometries {
			name := geometry.Label()
			if name == "" {
				name = unnamedWorldStateGeometryPrefix + strconv.Itoa(unnamedCount)
				geometry.SetLabel(name)
				unnamedCount++
			}

			if _, present := ws.obstacleNames[name]; present {
				return nil, NewDuplicateGeometryNameError(name)
			}
			ws.obstacleNames[name] = true
			checkedGeometries = append(checkedGeometries, geometry)
		}
		ws.obstacles = append(ws.obstacles, NewGeometriesInFrame(gf.frame, checkedGeometries))
	}
	return ws, nil
}

// WorldStateFromProtobuf takes the protobuf definition of a WorldState and converts it to a rdk defined WorldState.
func WorldStateFromProtobuf(proto *commonpb.WorldState) (*WorldState, error) {
	transforms, err := LinkInFramesFromTransformsProtobuf(proto.GetTransforms())
	if err != nil {
		return nil, err
	}

	allGeometries := make([]*GeometriesInFrame, 0)
	for _, protoGeometries := range proto.GetObstacles() {
		geometries, err := ProtobufToGeometriesInFrame(protoGeometries)
		if err != nil {
			return nil, err
		}
		allGeometries = append(allGeometries, geometries)
	}

	return NewWorldState(allGeometries, transforms)
}

// ToProtobuf takes an rdk WorldState and converts it to the protobuf definition of a WorldState.
func (ws *WorldState) ToProtobuf() (*commonpb.WorldState, error) {
	if ws == nil {
		return &commonpb.WorldState{}, nil
	}

	convertGeometriesToProto := func(allGeometries []*GeometriesInFrame) []*commonpb.GeometriesInFrame {
		list := make([]*commonpb.GeometriesInFrame, 0, len(allGeometries))
		for _, geometries := range allGeometries {
			list = append(list, GeometriesInFrameToProtobuf(geometries))
		}
		return list
	}

	transforms, err := LinkInFramesToTransformsProtobuf(ws.transforms)
	if err != nil {
		return nil, err
	}

	return &commonpb.WorldState{
		Obstacles:  convertGeometriesToProto(ws.obstacles),
		Transforms: transforms,
	}, nil
}

// MarshalJSON serializes an instance of WorldState to JSON through its protobuf representation.
func (ws *WorldState) MarshalJSON() ([]byte, error) {
	wsProto, err := ws.ToProtobuf()
	if err != nil {
		return nil, err
	}
	return protojson.Marshal(wsProto)
}

// UnmarshalJSON takes JSON bytes of a world state protobuf message and parses it
// into an instance of WorldState.
func (ws *WorldState) UnmarshalJSON(data []byte) error {
	var wsProto commonpb.WorldState
	if err := protojson.Unmarshal(data, &wsProto); err != nil {
		return err
	}
	newWs, err := WorldStateFromProtobuf(&wsProto)
	if err != nil {
		return err
	}
	*ws = *newWs
	return nil
}

// String returns a string representation of the geometries in the WorldState.
func (ws *WorldState) String() string {
	if ws == nil {
		return ""
	}

	t := table.NewWriter()
	t.AppendHeader(table.Row{"Name", "Geometry Type", "Parent"})
	for _, geometries := range ws.obstacles {
		for _, geometry := range geometries.geometries {
			t.AppendRow([]interface{}{
				geometry.Label(),
				fmt.Sprint(geometry),
				geometries.frame,
			})
		}
	}
	return t.Render()
}

// ObstacleNames returns the set of geometry names that have been registered in the WorldState, represented as a map.
func (ws *WorldState) ObstacleNames() map[string]bool {
	if ws == nil {
		return map[string]bool{}
	}

	copiedMap := make(map[string]bool)
	for key, value := range ws.obstacleNames {
		copiedMap[key] = value
	}
	return copiedMap
}

// Obstacles returns the obstacles that have been added to the WorldState.
func (ws *WorldState) Obstacles() []*GeometriesInFrame {
	if ws == nil {
		return []*GeometriesInFrame{}
	}
	return ws.obstacles
}

// Transforms returns the transforms that have been added to the WorldState.
func (ws *WorldState) Transforms() []*LinkInFrame {
	if ws == nil {
		return []*LinkInFrame{}
	}
	return ws.transforms
}

// ObstaclesInWorldFrame takes a frame system and a set of inputs for that frame system and converts all the obstacles
// in the WorldState such that they are in the frame system's World reference frame.
func (ws *WorldState) ObstaclesInWorldFrame(fs *FrameSystem, inputs FrameSystemInputs) (*GeometriesInFrame, error) {
	if ws == nil {
		return NewGeometriesInFrame(World, []spatialmath.Geometry{}), nil
	}

	allGeometries := make([]spatialmath.Geometry, 0, len(ws.obstacles))
	for _, gf := range ws.obstacles {
		tf, err := fs.Transform(inputs, gf, World)
		if err != nil {
			return nil, err
		}
		allGeometries = append(allGeometries, tf.(*GeometriesInFrame).Geometries()...)
	}
	return NewGeometriesInFrame(World, allGeometries), nil
}
