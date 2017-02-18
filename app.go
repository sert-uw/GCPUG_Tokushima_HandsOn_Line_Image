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

// 初期化処理
func init() {
	// line.envの読み込み
	err := godotenv.Load("line.env")
	if err != nil {
		panic(err)
	}

	// storage.envの読み込み
	err = godotenv.Load("storage.env")
	if err != nil {
		panic(err)
	}

	// storage.envにあるBUCKET_NAMEの設定値を取得
	bucketName = os.Getenv("BUCKET_NAME")

	// lineのhttphandlerを設定
	botHandler, err = httphandler.New(
		os.Getenv("LINE_BOT_CHANNEL_SECRET"),
		os.Getenv("LINE_BOT_CHANNEL_TOKEN"),
	)
	botHandler.HandleEvents(handleCallback)

	http.Handle("/callback", botHandler)
	http.HandleFunc("/task", handleTask)
}

// Webhook の受付関数
func handleCallback(evs []*linebot.Event, r *http.Request) {
	// 受信したイベントの逐次処理
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

// 受信したメッセージへの返信処理
func handleTask(w http.ResponseWriter, r *http.Request) {
	c := newContext(r)
	data := r.FormValue("data")

	if data == "" {
		log.Errorf(c, "No data")
		return
	}

	// メッセージJSONのパース
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

	// LINE bot 変数の生成
	bot, err := newLINEBot(c)
	if err != nil {
		log.Errorf(c, "newLINEBot: %v", err)
		return
	}

	log.Infof(c, "EventType: %s\nMessage: %#v", e.Type, e.Message)
	var responseMessage linebot.Message

	// 受信したメッセージのタイプチェック
	switch message := e.Message.(type) {
	// テキストメッセージの場合
	case *linebot.TextMessage:
		responseMessage = linebot.NewTextMessage(message.Text)

	// 画像の場合
	case *linebot.ImageMessage:
		// 画像情報の取得
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

		// エラーチェック
		if writeError1 == nil && writeError2 == nil {
			// 画像メッセージの生成
			origURL := bucketURLBase + bucketName + "/" + origPath
			thumbnailURL := bucketURLBase + bucketName + "/" + thumbnailPath
			responseMessage = linebot.NewImageMessage(origURL, thumbnailURL)
		} else {
			log.Errorf(c, "Write Error1: %v", writeError1)
			log.Errorf(c, "Write Error2: %v", writeError2)
			// テキストメッセージの生成
			responseMessage = linebot.NewTextMessage("失敗しました。。。")
		}
	default:
		responseMessage = linebot.NewTextMessage("未対応です。。。")
	}

	// 生成したメッセージを送信する
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

// LINEのMessageContentからimage.Imageを生成する
func decodeImage(content *linebot.MessageContentResponse) (image.Image, error) {
	var img image.Image
	var err error

	// 画像のフォーマットで場合分け
	if content.ContentType == "image/jpeg" {
		img, err = jpeg.Decode(content.Content)
	} else if content.ContentType == "image/png" {
		img, err = png.Decode(content.Content)
	}

	return img, err
}

// 画像をグレースケール化する
func convertToGray(img image.Image) image.Image {
	bounds := img.Bounds()
	// 空の画像を生成
	dest := image.NewGray16(bounds)
	// 元画像の各ピクセル値を取得し、グレースケール化する
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			c := color.Gray16Model.Convert(img.At(x, y))
			gray, _ := c.(color.Gray16)
			dest.Set(x, y, gray)
		}
	}
	return dest
}

// 画像をCloud Storageへ書き込む
func writeImage(c context.Context, img image.Image, path string) error {
	// storageのclientを生成する
	client, err := storage.NewClient(c)
	if err != nil {
		return err
	}

	// 画像をjpegのbyte配列へ変換する
	buf := new(bytes.Buffer)
	err = jpeg.Encode(buf, img, nil)
	if err != nil {
		return err
	}
	b := buf.Bytes()

	// Writerの生成
	writer := client.Bucket(bucketName).Object(path).NewWriter(c)
	// 画像のフォーマットはjpegとする
	writer.ContentType = "image/jpeg"
	// Cloud Storageのアクセス権限を一般公開に設定する
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
