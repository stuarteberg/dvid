package dvid

import (
	"fmt"
	"strings"
)

// Geometry describes the shape, size, and position of data in the DVID volume.
type Geometry interface {
	// DataShape describes the shape of the data.
	DataShape() DataShape

	// Size returns the extent in each dimension.
	Size() Point

	// NumVoxels returns the number of voxels within this space.
	NumVoxels() int64

	// StartPoint returns the offset to first point of data.
	StartPoint() Point

	// EndPoint returns the last point.
	EndPoint() Point

	String() string
}

type Dimension struct {
	Name     string
	Units    string
	beg, end int32
}

var (
	// XY describes a 2d rectangle of voxels that share a z-coord.
	XY = DataShape{3, []uint8{0, 1}}

	// XZ describes a 2d rectangle of voxels that share a y-coord.
	XZ = DataShape{3, []uint8{0, 2}}

	// YZ describes a 2d rectangle of voxels that share a x-coord.
	YZ = DataShape{3, []uint8{1, 2}}

	// Arb describes a 2d rectangle of voxels with arbitrary 3d orientation.
	Arb = DataShape{dims: 3}

	// Vol3d describes a 3d volume of voxels
	Vol3d = DataShape{3, []uint8{0, 1, 2}}
)

// DataShape describes the number of dimensions and the ordering of the dimensions.
type DataShape struct {
	dims  uint8
	shape []uint8
}

// BytesToDataShape recovers a DataShape from a series of bytes.
func BytesToDataShape(b []byte) (s DataShape, err error) {
	if b == nil {
		err = fmt.Errorf("Cannot convert nil to DataShape!")
		return
	}
	if len(b) != 6 {
		err = fmt.Errorf("Cannot convert %d bytes to DataShape", len(b))
		return
	}
	s = DataShape{dims: uint8(b[0])}
	n := int(s.dims)
	s.shape = make([]uint8, n)
	for i := 0; i < n; i++ {
		s.shape[i] = b[i+1]
	}
	return
}

// Bytes returns a fixed length byte representation that can be used for keys.
// Up to 5-d shapes can be used.
func (s DataShape) Bytes() []byte {
	b := make([]byte, 6)
	b[0] = byte(s.dims)
	n := int(s.dims)
	for i := 0; i < n; i++ {
		b[i+1] = s.shape[i]
	}
	return b
}

// TotalDimensions returns the full dimensionality of space within which there is this DataShape.
func (s DataShape) TotalDimensions() uint8 {
	return s.dims
}

// ShapeDimensions returns the number of dimensions for this shape.
func (s DataShape) ShapeDimensions() uint8 {
	if s.shape == nil {
		return 0
	}
	return uint8(len(s.shape))
}

// Duplicate returns a duplicate of the DataShape.
func (s DataShape) Duplicate() DataShape {
	dup := DataShape{dims: s.dims}
	copy(dup.shape, s.shape)
	return dup
}

// Equals returns true if the passed DataShape is identical.
func (s DataShape) Equals(s2 DataShape) bool {
	if s.dims == s2.dims {
		for i, dim := range s.shape {
			if s2.shape[i] != dim {
				return false
			}
		}
		return true
	}
	return false
}

func (s DataShape) String() string {
	switch {
	case s.Equals(XY):
		return "XY slice"
	case s.Equals(XZ):
		return "XZ slice"
	case s.Equals(YZ):
		return "YZ slice"
	case s.Equals(Arb):
		return "slice with arbitrary orientation"
	case s.Equals(Vol3d):
		return "3d volume"
	case s.dims > 3:
		return "n-D volume"
	default:
		return "Unknown shape"
	}
}

// String for specifying a slice orientation or subvolume
type DataShapeString string

// List of strings associated with shapes up to 3d
var dataShapeStrings = map[string]DataShape{
	"xy":    XY,
	"xz":    XZ,
	"yz":    YZ,
	"vol":   Vol3d,
	"arb":   Arb,
	"0_1":   XY,
	"0_2":   XZ,
	"1_2":   YZ,
	"0_1_2": Vol3d,
}

// ListDataShapes returns a slice of shape names
func ListDataShapes() (shapes []string) {
	shapes = []string{}
	for key, _ := range dataShapeStrings {
		shapes = append(shapes, string(key))
	}
	return
}

// DataShape returns the data shape constant associated with the string.
func (s DataShapeString) DataShape() (shape DataShape, err error) {
	shape, found := dataShapeStrings[strings.ToLower(string(s))]
	if !found {
		err = fmt.Errorf("Unknown data shape specification (%s)", s)
	}
	return
}

// ---- Geometry implementations ------

// Subvolume describes a 3d box Geometry.  The "Sub" prefix emphasizes that the
// data is usually a smaller portion of the volume held by the DVID datastore.
// Note that the 3d coordinate system is assumed to be a Right-Hand system like OpenGL.
type Subvolume struct {
	shape  DataShape
	offset Point
	size   Point
}

// NewSubvolumeFromStrings returns a Subvolume given string representations of
// offset ("0,10,20") and size ("250,250,250").
func NewSubvolumeFromStrings(offsetStr, sizeStr string) (v *Subvolume, err error) {
	offset, err := StringToPoint(offsetStr, ",")
	if err != nil {
		return
	}
	size, err := StringToPoint(sizeStr, ",")
	if err != nil {
		return
	}
	v = NewSubvolume(offset, size)
	return
}

// NewSubvolume returns a Subvolume given a subvolume's origin and size.
func NewSubvolume(offset, size Point) *Subvolume {
	return &Subvolume{Vol3d, offset, size}
}

func (s *Subvolume) DataShape() DataShape {
	return Vol3d
}

func (s *Subvolume) Size() Point {
	return s.size
}

func (s *Subvolume) NumVoxels() int64 {
	if s == nil || s.size.NumDims() == 0 {
		return 0
	}
	voxels := int64(s.size.Value(0))
	for dim := uint8(1); dim < s.size.NumDims(); dim++ {
		voxels *= int64(s.size.Value(dim))
	}
	return voxels
}

func (s *Subvolume) StartPoint() Point {
	return s.offset
}

func (s *Subvolume) EndPoint() Point {
	return s.offset.Add(s.size.Sub(Point3d{-1, -1, -1}))
}

func (s *Subvolume) String() string {
	return fmt.Sprintf("%s %s at offset %s", s.shape, s.size, s.offset)
}

// OrthogSlice is a 2d rectangle orthogonal to two axis of the space that is slices.
// It fulfills a Geometry interface.
type OrthogSlice struct {
	shape    DataShape
	offset   Point
	size     Point2d
	endPoint Point
}

// NewSliceFromStrings returns a Geometry object for a XY, XZ, or YZ slice given
// a data shape string, offset ("0,10,20"), and size ("250,250").
func NewSliceFromStrings(str DataShapeString, offsetStr, sizeStr string) (slice Geometry, err error) {
	shape, err := str.DataShape()
	if err != nil {
		return
	}
	offset, err := StringToPoint(offsetStr, ",")
	if err != nil {
		return
	}
	// Enforce that size string is 2d since this is supposed to be a slice.
	ndstring, err := StringToNdString(sizeStr, ",")
	if err != nil {
		return nil, err
	}
	size, err := ndstring.Point2d()
	if err != nil {
		return nil, err
	}
	return NewOrthogSlice(shape, offset, size)
}

// NewOrthogSlice returns an OrthogSlice of chosen orientation, offset, and size.
func NewOrthogSlice(s DataShape, offset Point, size Point2d) (Geometry, error) {
	if offset.NumDims() != s.dims {
		return nil, fmt.Errorf("NewOrthogSlice: offset dimensionality %d != shape %d",
			offset.NumDims(), s.dims)
	}
	if s.shape == nil || len(s.shape) != 2 {
		return nil, fmt.Errorf("NewOrthogSlice: shape not properly specified")
	}
	xDim := s.shape[0]
	if xDim >= s.dims {
		return nil, fmt.Errorf("NewOrthogSlice: X dimension of slice (%d) > # avail dims (%d)",
			xDim, s.dims)
	}
	yDim := s.shape[1]
	if yDim >= s.dims {
		return nil, fmt.Errorf("NewOrthogSlice: Y dimension of slice (%d) > # avail dims (%d)",
			yDim, s.dims)
	}
	settings := map[uint8]int32{
		xDim: size[0],
		yDim: size[1],
	}
	geom := &OrthogSlice{
		shape:    s,
		offset:   offset.Duplicate(),
		size:     size,
		endPoint: offset.Modify(settings),
	}
	return geom, nil
}

func (s OrthogSlice) DataShape() DataShape {
	return s.shape
}

func (s OrthogSlice) Size() Point {
	return s.size
}

func (s OrthogSlice) NumVoxels() int64 {
	return int64(s.size[0] * s.size[1])
}

func (s OrthogSlice) StartPoint() Point {
	return s.offset
}

func (s OrthogSlice) EndPoint() Point {
	return s.endPoint
}

func (s OrthogSlice) String() string {
	return fmt.Sprintf("%s @ offset %s, size %s", s.shape, s.offset, s.size)
}
