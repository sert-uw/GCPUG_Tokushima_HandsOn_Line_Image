package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/url"
	"os"

	"google.golang.org/appengine"
	"google.golang.org/appengine/log"
	"google.golang.org/appengine/taskqueue"
	"google.golang.org/appengine/urlfetch"

	"golang.org/x/net/context"

	"cloud.google.com/go/storage"
	"github.com/joho/godotenv"
	"github.com/line/line-bot-sdk-go/linebot"
	"github.com/line/line-bot-sdk-go/linebot/httphandler"
	"github.com/nfnt/resize"
)

const bucketURLBase = "https://storage.googleapis.com/"

var botHandler *httphandler.WebhookHandler
var bucketName string

func init() {
	err := godotenv.Load("line.env")
	if err != nil {
		panic(err)
	}
	err = godotenv.Load("storage.env")
	if err != nil {
		panic(err)
	}

	bucketName = os.Getenv("BUCKET_NAME")

	botHandler, err = httphandler.New(
		os.Getenv("LINE_BOT_CHANNEL_SECRET"),
		os.Getenv("LINE_BOT_CHANNEL_TOKEN"),
	)
	botHandler.HandleEvents(handleCallback)

	http.Handle("/callback", botHandler)
	http.HandleFunc("/task", handleTask)
}

// handleCallback is Webgook endpoint
func handleCallback(evs []*linebot.Event, r *http.Request) {
	c := newContext(r)
	ts := make([]*taskqueue.Task, len(evs))
	for i, e := range evs {
		j, err := json.Marshal(e)
		if err != nil {
			log.Errorf(c, "json.Marshal: %v", err)
			return
		}
		data := base64.StdEncoding.EncodeToString(j)
		t := taskqueue.NewPOSTTask("/task", url.Values{"data": {data}})
		ts[i] = t
	}
	taskqueue.AddMulti(c, ts, "")
}

// handleTask is process event handler
func handleTask(w http.ResponseWriter, r *http.Request) {
	c := newContext(r)
	data := r.FormValue("data")

	if data == "" {
		log.Errorf(c, "No data")
		return
	}

	j, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		log.Errorf(c, "base64 DecodeString: %v", err)
		return
	}

	e := new(linebot.Event)
	err = json.Unmarshal(j, e)
	if err != nil {
		log.Errorf(c, "json.Unmarshal: %v", err)
		return
	}

	bot, err := newLINEBot(c)
	if err != nil {
		log.Errorf(c, "newLINEBot: %v", err)
		return
	}

	log.Infof(c, "EventType: %s\nMessage: %#v", e.Type, e.Message)
	var responseMessage linebot.Message

	switch message := e.Message.(type) {
	case *linebot.TextMessage:
		responseMessage = linebot.NewTextMessage(message.Text)
	case *linebot.ImageMessage:
		// 画像の取得
		content, err := bot.GetMessageContent(message.ID).Do()
		if err != nil {
			log.Errorf(c, "Load error: %v", err)
			return
		}
		defer content.Content.Close()

		// バイナリデータからimage.Imageを生成
		img, err2 := decodeImage(content)
		if err2 != nil {
			log.Errorf(c, "Load error: %v", err2)
			return
		}

		// グレースケール化
		img = convertToGray(img)

		// サムネイルの作成
		thumbnail := resize.Thumbnail(300, 300, img, resize.Lanczos3)

		// Cloud Storageのパス設定
		origPath := "images/" + message.ID + ".jpg"
		thumbnailPath := "thumbnails/" + message.ID + ".jpg"

		// Cloud Storageへ書き込み
		writeError1 := writeImage(c, img, origPath)
		writeError2 := writeImage(c, thumbnail, thumbnailPath)

		if writeError1 == nil && writeError2 == nil {
			origURL := bucketURLBase + bucketName + "/" + origPath
			thumbnailURL := bucketURLBase + bucketName + "/" + thumbnailPath
			responseMessage = linebot.NewImageMessage(origURL, thumbnailURL)
		} else {
			log.Errorf(c, "Write Error1: %v", writeError1)
			log.Errorf(c, "Write Error2: %v", writeError2)
			responseMessage = linebot.NewTextMessage("失敗しました。。。")
		}
	default:
		responseMessage = linebot.NewTextMessage("未対応です。。。")
	}

	if _, err = bot.ReplyMessage(e.ReplyToken, responseMessage).WithContext(c).Do(); err != nil {
		log.Errorf(c, "ReplayMessage: %v", err)
		return
	}

	w.WriteHeader(200)
}

func newContext(r *http.Request) context.Context {
	return appengine.NewContext(r)
}

func newLINEBot(c context.Context) (*linebot.Client, error) {
	return botHandler.NewClient(
		linebot.WithHTTPClient(urlfetch.Client(c)),
	)
}

func decodeImage(content *linebot.MessageContentResponse) (image.Image, error) {
	var img image.Image
	var err error

	if content.ContentType == "image/jpeg" {
		img, err = jpeg.Decode(content.Content)
	} else if content.ContentType == "image/png" {
		img, err = png.Decode(content.Content)
	}

	return img, err
}

func convertToGray(img image.Image) image.Image {
	bounds := img.Bounds()
	dest := image.NewGray16(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			c := color.Gray16Model.Convert(img.At(x, y))
			gray, _ := c.(color.Gray16)
			dest.Set(x, y, gray)
		}
	}
	return dest
}

func writeImage(c context.Context, img image.Image, path string) error {
	client, err := storage.NewClient(c)
	if err != nil {
		return err
	}

	buf := new(bytes.Buffer)
	err = jpeg.Encode(buf, img, nil)
	if err != nil {
		return err
	}
	b := buf.Bytes()

	// Writerの生成
	writer := client.Bucket(bucketName).Object(path).NewWriter(c)
	writer.ContentType = "image/jpeg"
	writer.ObjectAttrs.ACL = []storage.ACLRule{
		storage.ACLRule{
			Entity: storage.AllUsers,
			Role:   storage.RoleReader,
		},
	}

	defer writer.Close()

	// 画像を書き込む
	if _, err := writer.Write(b); err != nil {
		return err
	}

	return nil
}
