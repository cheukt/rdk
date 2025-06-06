package camera_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"sync"
	"testing"
	"time"

	pb "go.viam.com/api/component/camera/v1"
	"go.viam.com/test"
	goprotoutils "go.viam.com/utils/protoutils"

	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/pointcloud"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/rimage"
	"go.viam.com/rdk/rimage/transform"
	"go.viam.com/rdk/testutils/inject"
	"go.viam.com/rdk/utils"
)

var (
	errInvalidMimeType          = errors.New("invalid mime type")
	errGeneratePointCloudFailed = errors.New("can't generate next point cloud")
	errPropertiesFailed         = errors.New("can't get camera properties")
	errCameraProjectorFailed    = errors.New("can't get camera properties")
	errGetImageFailed           = errors.New("can't get image")
	errCameraUnimplemented      = errors.New("not found")
)

func newServer() (pb.CameraServiceServer, *inject.Camera, *inject.Camera, *inject.Camera, error) {
	injectCamera := &inject.Camera{}
	injectCameraDepth := &inject.Camera{}
	injectCamera2 := &inject.Camera{}
	cameras := map[resource.Name]camera.Camera{
		camera.Named(testCameraName):  injectCamera,
		camera.Named(depthCameraName): injectCameraDepth,
		camera.Named(failCameraName):  injectCamera2,
	}
	cameraSvc, err := resource.NewAPIResourceCollection(camera.API, cameras)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return camera.NewRPCServiceServer(cameraSvc).(pb.CameraServiceServer), injectCamera, injectCameraDepth, injectCamera2, nil
}

func TestServer(t *testing.T) {
	cameraServer, injectCamera, injectCameraDepth, injectCamera2, err := newServer()
	test.That(t, err, test.ShouldBeNil)

	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	var imgBuf bytes.Buffer
	test.That(t, png.Encode(&imgBuf, img), test.ShouldBeNil)
	var imgBufJpeg bytes.Buffer

	test.That(t, jpeg.Encode(&imgBufJpeg, img, &jpeg.Options{Quality: 75}), test.ShouldBeNil)

	imgPng, err := png.Decode(bytes.NewReader(imgBuf.Bytes()))
	test.That(t, err, test.ShouldBeNil)

	imgJpeg, err := jpeg.Decode(bytes.NewReader(imgBufJpeg.Bytes()))

	test.That(t, err, test.ShouldBeNil)

	var projA transform.Projector
	intrinsics := &transform.PinholeCameraIntrinsics{ // not the real camera parameters -- fake for test
		Width:  1280,
		Height: 720,
		Fx:     200,
		Fy:     200,
		Ppx:    100,
		Ppy:    100,
	}
	projA = intrinsics
	pcA := pointcloud.NewBasicEmpty()
	err = pcA.Set(pointcloud.NewVector(5, 5, 5), nil)
	test.That(t, err, test.ShouldBeNil)

	injectCamera.NextPointCloudFunc = func(ctx context.Context) (pointcloud.PointCloud, error) {
		return pcA, nil
	}
	injectCamera.PropertiesFunc = func(ctx context.Context) (camera.Properties, error) {
		return camera.Properties{
			SupportsPCD:     true,
			IntrinsicParams: intrinsics,
			MimeTypes:       []string{utils.MimeTypeJPEG, utils.MimeTypePNG, utils.MimeTypeH264},
			FrameRate:       float32(10.0),
		}, nil
	}
	injectCamera.ImagesFunc = func(ctx context.Context) ([]camera.NamedImage, resource.ResponseMetadata, error) {
		images := []camera.NamedImage{}
		// one color image
		color := rimage.NewImage(40, 50)
		images = append(images, camera.NamedImage{color, "color"})
		// one depth image
		depth := rimage.NewEmptyDepthMap(10, 20)
		images = append(images, camera.NamedImage{depth, "depth"})
		// a timestamp of 12345
		ts := time.UnixMilli(12345)
		return images, resource.ResponseMetadata{ts}, nil
	}
	injectCamera.ProjectorFunc = func(ctx context.Context) (transform.Projector, error) {
		return projA, nil
	}
	wooMIME := "image/woohoo"
	emptyMIME := "image/empty"
	injectCamera.ImageFunc = func(ctx context.Context, mimeType string, extra map[string]interface{}) ([]byte, camera.ImageMetadata, error) {
		if mimeType == "" {
			mimeType = utils.MimeTypeRawRGBA
		}
		mimeType, _ = utils.CheckLazyMIMEType(mimeType)

		var respImg image.Image
		switch mimeType {
		case "", utils.MimeTypeRawRGBA:
			respImg = img
		case utils.MimeTypePNG:
			respImg = imgPng
		case utils.MimeTypeJPEG:
			respImg = imgJpeg
		case wooMIME:
			respImg = rimage.NewLazyEncodedImage([]byte{1, 2, 3}, mimeType)
		case emptyMIME:
			return []byte{}, camera.ImageMetadata{}, nil
		default:
			return nil, camera.ImageMetadata{}, errInvalidMimeType
		}
		resBytes, err := rimage.EncodeImage(ctx, respImg, mimeType)
		if err != nil {
			return nil, camera.ImageMetadata{}, err
		}
		return resBytes, camera.ImageMetadata{MimeType: mimeType}, nil
	}
	// depth camera
	depthImage := rimage.NewEmptyDepthMap(10, 20)
	depthImage.Set(0, 0, rimage.Depth(40))
	depthImage.Set(0, 1, rimage.Depth(1))
	depthImage.Set(5, 6, rimage.Depth(190))
	depthImage.Set(9, 12, rimage.Depth(3000))
	depthImage.Set(5, 9, rimage.MaxDepth-rimage.Depth(1))
	var depthBuf bytes.Buffer
	test.That(t, png.Encode(&depthBuf, depthImage), test.ShouldBeNil)
	injectCameraDepth.NextPointCloudFunc = func(ctx context.Context) (pointcloud.PointCloud, error) {
		return pcA, nil
	}
	// no frame rate camera
	injectCameraDepth.PropertiesFunc = func(ctx context.Context) (camera.Properties, error) {
		return camera.Properties{
			SupportsPCD:     true,
			IntrinsicParams: intrinsics,
			ImageType:       camera.DepthStream,
		}, nil
	}
	injectCameraDepth.ProjectorFunc = func(ctx context.Context) (transform.Projector, error) {
		return projA, nil
	}
	injectCameraDepth.ImageFunc = func(
		ctx context.Context,
		mimeType string,
		extra map[string]interface{},
	) ([]byte, camera.ImageMetadata, error) {
		resBytes, err := rimage.EncodeImage(ctx, depthImage, mimeType)
		if err != nil {
			return nil, camera.ImageMetadata{}, err
		}
		return resBytes, camera.ImageMetadata{MimeType: mimeType}, nil
	}
	// bad camera
	injectCamera2.NextPointCloudFunc = func(ctx context.Context) (pointcloud.PointCloud, error) {
		return nil, errGeneratePointCloudFailed
	}
	injectCamera2.PropertiesFunc = func(ctx context.Context) (camera.Properties, error) {
		return camera.Properties{}, errPropertiesFailed
	}
	injectCamera2.ProjectorFunc = func(ctx context.Context) (transform.Projector, error) {
		return nil, errCameraProjectorFailed
	}
	injectCamera2.ImageFunc = func(ctx context.Context, mimeType string, extra map[string]interface{}) ([]byte, camera.ImageMetadata, error) {
		return nil, camera.ImageMetadata{}, errGetImageFailed
	}

	// does a depth camera transfer its depth image properly
	t.Run("GetImage", func(t *testing.T) {
		_, err := cameraServer.GetImage(context.Background(), &pb.GetImageRequest{Name: missingCameraName})
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, errCameraUnimplemented.Error())

		// color camera
		// ensure that explicit RawRGBA mimetype request will return RawRGBA mimetype response
		resp, err := cameraServer.GetImage(
			context.Background(),
			&pb.GetImageRequest{Name: testCameraName, MimeType: utils.MimeTypeRawRGBA},
		)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, resp.MimeType, test.ShouldEqual, utils.MimeTypeRawRGBA)
		test.That(t, resp.Image[rimage.RawRGBAHeaderLength:], test.ShouldResemble, img.Pix)

		// ensure that empty mimetype request from color cam will return JPEG mimetype response
		resp, err = cameraServer.GetImage(
			context.Background(),
			&pb.GetImageRequest{Name: testCameraName, MimeType: ""},
		)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, resp.MimeType, test.ShouldEqual, utils.MimeTypeJPEG)
		test.That(t, resp.Image, test.ShouldNotBeNil)

		// ensure that empty mimetype request from depth cam will return PNG mimetype response
		resp, err = cameraServer.GetImage(
			context.Background(),
			&pb.GetImageRequest{Name: depthCameraName, MimeType: ""},
		)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, resp.MimeType, test.ShouldEqual, utils.MimeTypeRawDepth)
		test.That(t, resp.Image, test.ShouldNotBeNil)

		resp, err = cameraServer.GetImage(context.Background(), &pb.GetImageRequest{
			Name:     testCameraName,
			MimeType: "image/png",
		})
		test.That(t, err, test.ShouldBeNil)
		test.That(t, resp.MimeType, test.ShouldEqual, utils.MimeTypePNG)
		test.That(t, resp.Image, test.ShouldResemble, imgBuf.Bytes())

		_, err = cameraServer.GetImage(context.Background(), &pb.GetImageRequest{
			Name:     testCameraName,
			MimeType: "image/who",
		})
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, errInvalidMimeType.Error())

		// depth camera
		resp, err = cameraServer.GetImage(
			context.Background(),
			&pb.GetImageRequest{Name: depthCameraName, MimeType: utils.MimeTypePNG},
		)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, resp.MimeType, test.ShouldEqual, utils.MimeTypePNG)
		test.That(t, resp.Image, test.ShouldNotBeNil)
		decodedDepth, err := rimage.DecodeImage(
			context.Background(),
			resp.Image,
			resp.MimeType,
		)
		test.That(t, err, test.ShouldBeNil)
		dm, err := rimage.ConvertImageToDepthMap(context.Background(), decodedDepth)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, dm, test.ShouldResemble, depthImage)

		resp, err = cameraServer.GetImage(context.Background(), &pb.GetImageRequest{
			Name:     depthCameraName,
			MimeType: "image/png",
		})
		test.That(t, err, test.ShouldBeNil)
		test.That(t, resp.MimeType, test.ShouldEqual, utils.MimeTypePNG)
		test.That(t, resp.Image, test.ShouldResemble, depthBuf.Bytes())
		// bad camera
		_, err = cameraServer.GetImage(context.Background(), &pb.GetImageRequest{Name: failCameraName, MimeType: utils.MimeTypeRawRGBA})
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, errGetImageFailed.Error())
	})

	t.Run("GetImage response image bytes empty", func(t *testing.T) {
		_, err := cameraServer.GetImage(context.Background(), &pb.GetImageRequest{
			Name:     testCameraName,
			MimeType: emptyMIME,
		})
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, "received empty bytes from Image method")
	})

	t.Run("GetImage with lazy", func(t *testing.T) {
		// we know its lazy if it's a mime we can't actually handle internally
		resp, err := cameraServer.GetImage(context.Background(), &pb.GetImageRequest{
			Name:     testCameraName,
			MimeType: wooMIME,
		})
		test.That(t, err, test.ShouldBeNil)
		test.That(t, resp.MimeType, test.ShouldEqual, wooMIME)
		test.That(t, resp.Image, test.ShouldResemble, []byte{1, 2, 3})

		_, err = cameraServer.GetImage(context.Background(), &pb.GetImageRequest{
			Name:     testCameraName,
			MimeType: "image/notwoo",
		})
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, errInvalidMimeType.Error())
	})

	t.Run("GetImage with +lazy default", func(t *testing.T) {
		for _, mimeType := range []string{
			utils.MimeTypePNG,
			utils.MimeTypeJPEG,
			utils.MimeTypeRawRGBA,
		} {
			req := pb.GetImageRequest{
				Name:     testCameraName,
				MimeType: mimeType,
			}
			resp, err := cameraServer.GetImage(context.Background(), &req)
			test.That(t, err, test.ShouldBeNil)
			test.That(t, resp.Image, test.ShouldNotBeNil)
			test.That(t, req.MimeType, test.ShouldEqual, utils.WithLazyMIMEType(mimeType))
		}
	})

	t.Run("RenderFrame", func(t *testing.T) {
		_, err := cameraServer.RenderFrame(context.Background(), &pb.RenderFrameRequest{Name: missingCameraName})
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, errCameraUnimplemented.Error())

		resp, err := cameraServer.RenderFrame(context.Background(), &pb.RenderFrameRequest{
			Name: testCameraName,
		})
		test.That(t, err, test.ShouldBeNil)
		test.That(t, resp.ContentType, test.ShouldEqual, "image/jpeg")
		test.That(t, resp.Data, test.ShouldResemble, imgBufJpeg.Bytes())

		resp, err = cameraServer.RenderFrame(context.Background(), &pb.RenderFrameRequest{
			Name:     testCameraName,
			MimeType: "image/png",
		})
		test.That(t, err, test.ShouldBeNil)
		test.That(t, resp.ContentType, test.ShouldEqual, "image/png")
		test.That(t, resp.Data, test.ShouldResemble, imgBuf.Bytes())

		_, err = cameraServer.RenderFrame(context.Background(), &pb.RenderFrameRequest{
			Name:     testCameraName,
			MimeType: "image/who",
		})
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, errInvalidMimeType.Error())

		_, err = cameraServer.RenderFrame(context.Background(), &pb.RenderFrameRequest{Name: failCameraName})
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, errGetImageFailed.Error())
	})

	t.Run("GetPointCloud", func(t *testing.T) {
		_, err := cameraServer.GetPointCloud(context.Background(), &pb.GetPointCloudRequest{Name: missingCameraName})
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, errCameraUnimplemented.Error())

		pcA := pointcloud.NewBasicEmpty()
		err = pcA.Set(pointcloud.NewVector(5, 5, 5), nil)
		test.That(t, err, test.ShouldBeNil)

		injectCamera.NextPointCloudFunc = func(ctx context.Context) (pointcloud.PointCloud, error) {
			return pcA, nil
		}
		_, err = cameraServer.GetPointCloud(context.Background(), &pb.GetPointCloudRequest{
			Name: testCameraName,
		})
		test.That(t, err, test.ShouldBeNil)

		_, err = cameraServer.GetPointCloud(context.Background(), &pb.GetPointCloudRequest{
			Name: failCameraName,
		})
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, errGeneratePointCloudFailed.Error())
	})
	t.Run("GetImages", func(t *testing.T) {
		_, err := cameraServer.GetImages(context.Background(), &pb.GetImagesRequest{Name: missingCameraName})
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, errCameraUnimplemented.Error())

		resp, err := cameraServer.GetImages(context.Background(), &pb.GetImagesRequest{Name: testCameraName})
		test.That(t, err, test.ShouldBeNil)
		test.That(t, resp.ResponseMetadata.CapturedAt.AsTime(), test.ShouldEqual, time.UnixMilli(12345))
		test.That(t, len(resp.Images), test.ShouldEqual, 2)
		test.That(t, resp.Images[0].Format, test.ShouldEqual, pb.Format_FORMAT_JPEG)
		test.That(t, resp.Images[0].SourceName, test.ShouldEqual, "color")
		test.That(t, resp.Images[1].Format, test.ShouldEqual, pb.Format_FORMAT_RAW_DEPTH)
		test.That(t, resp.Images[1].SourceName, test.ShouldEqual, "depth")
	})

	t.Run("GetProperties", func(t *testing.T) {
		_, err := cameraServer.GetProperties(context.Background(), &pb.GetPropertiesRequest{Name: missingCameraName})
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, errCameraUnimplemented.Error())

		resp, err := cameraServer.GetProperties(context.Background(), &pb.GetPropertiesRequest{Name: testCameraName})
		test.That(t, err, test.ShouldBeNil)
		test.That(t, resp.SupportsPcd, test.ShouldBeTrue)
		test.That(t, resp.IntrinsicParameters.WidthPx, test.ShouldEqual, 1280)
		test.That(t, resp.IntrinsicParameters.HeightPx, test.ShouldEqual, 720)
		test.That(t, resp.IntrinsicParameters.FocalXPx, test.ShouldEqual, 200)
		test.That(t, resp.IntrinsicParameters.FocalYPx, test.ShouldEqual, 200)
		test.That(t, resp.IntrinsicParameters.CenterXPx, test.ShouldEqual, 100)
		test.That(t, resp.IntrinsicParameters.CenterYPx, test.ShouldEqual, 100)
		test.That(t, resp.MimeTypes, test.ShouldContain, utils.MimeTypeJPEG)
		test.That(t, resp.MimeTypes, test.ShouldContain, utils.MimeTypePNG)
		test.That(t, resp.MimeTypes, test.ShouldContain, utils.MimeTypeH264)
		test.That(t, resp.FrameRate, test.ShouldNotBeNil)
		test.That(t, *resp.FrameRate, test.ShouldEqual, 10.0)

		// test property when we don't set frame rate
		resp2, err := cameraServer.GetProperties(context.Background(), &pb.GetPropertiesRequest{Name: depthCameraName})
		test.That(t, err, test.ShouldBeNil)
		test.That(t, resp2.FrameRate, test.ShouldBeNil)
	})

	t.Run("GetImage with extra", func(t *testing.T) {
		injectCamera.ImageFunc = func(ctx context.Context, mimeType string, extra map[string]interface{}) ([]byte, camera.ImageMetadata, error) {
			test.That(t, extra, test.ShouldBeEmpty)
			return nil, camera.ImageMetadata{}, errGetImageFailed
		}

		_, err := cameraServer.GetImage(context.Background(), &pb.GetImageRequest{
			Name: testCameraName,
		})

		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, errGetImageFailed.Error())

		injectCamera.ImageFunc = func(ctx context.Context, mimeType string, extra map[string]interface{}) ([]byte, camera.ImageMetadata, error) {
			test.That(t, len(extra), test.ShouldEqual, 1)
			test.That(t, extra["hello"], test.ShouldEqual, "world")
			return nil, camera.ImageMetadata{}, errGetImageFailed
		}

		ext, err := goprotoutils.StructToStructPb(map[string]interface{}{"hello": "world"})
		test.That(t, err, test.ShouldBeNil)

		_, err = cameraServer.GetImage(context.Background(), &pb.GetImageRequest{
			Name:  testCameraName,
			Extra: ext,
		})

		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, errGetImageFailed.Error())

		injectCamera.ImageFunc = func(ctx context.Context, mimeType string, extra map[string]interface{}) ([]byte, camera.ImageMetadata, error) {
			test.That(t, len(extra), test.ShouldEqual, 1)
			test.That(t, extra[data.FromDMString], test.ShouldBeTrue)

			return nil, camera.ImageMetadata{}, errGetImageFailed
		}

		// one kvp created with data.FromDMContextKey
		ext, err = goprotoutils.StructToStructPb(map[string]interface{}{data.FromDMString: true})
		test.That(t, err, test.ShouldBeNil)

		_, err = cameraServer.GetImage(context.Background(), &pb.GetImageRequest{
			Name:  testCameraName,
			Extra: ext,
		})

		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, errGetImageFailed.Error())

		injectCamera.ImageFunc = func(ctx context.Context, mimeType string, extra map[string]interface{}) ([]byte, camera.ImageMetadata, error) {
			test.That(t, len(extra), test.ShouldEqual, 2)
			test.That(t, extra["hello"], test.ShouldEqual, "world")
			test.That(t, extra[data.FromDMString], test.ShouldBeTrue)
			return nil, camera.ImageMetadata{}, errGetImageFailed
		}

		// use values from data and camera
		ext, err = goprotoutils.StructToStructPb(
			map[string]interface{}{
				data.FromDMString: true,
				"hello":           "world",
			},
		)
		test.That(t, err, test.ShouldBeNil)

		_, err = cameraServer.GetImage(context.Background(), &pb.GetImageRequest{
			Name:  testCameraName,
			Extra: ext,
		})

		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, errGetImageFailed.Error())
	})
}

func TestGetImageRace(t *testing.T) {
	cameraServer, injectCamera, _, _, err := newServer()
	test.That(t, err, test.ShouldBeNil)

	injectCamera.PropertiesFunc = func(ctx context.Context) (camera.Properties, error) { return camera.Properties{}, nil }
	injectCamera.ImageFunc = func(ctx context.Context, mimeType string, extra map[string]interface{}) ([]byte, camera.ImageMetadata, error) {
		return []byte{1, 2}, camera.ImageMetadata{}, nil
	}
	var wg sync.WaitGroup

	getImg := func() {
		defer wg.Done()
		_, retErr := cameraServer.GetImage(context.Background(), &pb.GetImageRequest{Name: testCameraName})
		test.That(t, retErr, test.ShouldBeNil)
	}
	for range 2 {
		wg.Add(1)
		go getImg()
	}
	wg.Wait()
}
