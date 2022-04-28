package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/1set/gut/yos"
	"github.com/rjeczalik/notify"
	"github.com/spf13/viper"

	"github.com/malashin/ffinfo"

	tele "gopkg.in/telebot.v3"
)

func main() {
	viper.SetDefault("loglevel", "debug")
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")

	err := viper.ReadInConfig()
	if err != nil {
		log.Panicf("Unable to read config file: %s", err)
	}

	bot := NewTgBot()

	fsEnents := make(chan notify.EventInfo, 300)

	err = notify.Watch(
		viper.GetString("path.buffer.in"),
		fsEnents,
		notify.InCloseWrite,
	)
	if err != nil {
		log.Panicf("Unable to start notify wather: %s", err)
	}
	defer notify.Stop(fsEnents)

	var movieLock sync.Mutex
	var trlLock sync.Mutex

next_event:
	for {
		switch e := <-fsEnents; e.Event() {
		case notify.InCloseWrite:
			{
				// Существует какая-то ошибка, при которой "moov atom not found"
				// И с субтитрами тоже самое
				time.Sleep(2 * time.Second)
				srcPath := e.Path()

				if IsAmediaFile(srcPath) {
					continue
				}

				excludeExts := []string{
					"",
					".stl",
					".srt",
					".txt",
					".xml",
					".exe",

					".wav",
					".ac3",
					".m4a",

					".zip",
					".tar",
				}

				for _, ext := range excludeExts {
					if strings.ToLower(ext) == strings.ToLower(path.Ext(srcPath)) {
						continue next_event
					}
				}

				if IsTRL(srcPath) {
					go ProcessTrl(srcPath, bot, &trlLock)
				}
				//  else {
				// 	go ProcessMovie(srcPath, &movieLock)
				// }
			}
		}
	}
}

func IsAmediaFile(fn string) bool {
	var amediaRE = regexp.MustCompile(`^(?P<name>.*?)(_s(?P<season>\d{2}))?(e(?P<episode>\d{2,4}))?_(?P<type>MOV|SER|SPO)_(?P<id>\d*)(\.RUS)?(_R(?P<replace>\d))?(\.RUS)?\.(?P<ext>srt|mp4)$`)
	return amediaRE.MatchString(path.Base(fn))
}

func IsTRL(fn string) bool {
	if GetDuration(fn) >= 6*60 {
		return false
	}

	size, _ := yos.GetFileSize(fn)

	if size > 6_000_000_000 {
		return false
	}

	return true
}

func IsAllAudioChannelsMixed(info *ffinfo.File) bool {
	audioCount := 0

	for index, stream := range info.Streams {
		if stream.CodecType == "audio" {
			if stream.Channels != 6 && stream.Channels != 2 {
				return false
			}

			audioCount++
		}
	}

	if audioCount == 0 {
		return false
	}

	return true
}

func GetVideoStream(streams []ffinfo.Stream) *ffinfo.Stream {
	for _, stream := range streams {
		if stream.CodecType == "video" {
			return &stream
		}
	}

	return nil
}

func GetDuration(fn string) float64 {
	info, err := ffinfo.Probe(fn)

	if err != nil {
		log.Printf("ffprobe start error: %s", err)
		return 0.0
	}

	number, _ := strconv.ParseFloat(info.Format.Duration, 64)

	return number
}

func GetAllAudioStreams(info *ffinfo.File) []ffinfo.Stream {
	audioStreams := make([]ffinfo.Stream, 0, 0)

	for index, stream := range info.Streams {
		if stream.CodecType == "audio" {
			audioStreams = append(audioStreams, stream)
		}
	}

	return audioStreams
}

func GetAudioFileNameChannelsPart(audioStream *ffinfo.Stream) (audioFileNameChannelsPart string) {
	switch audioStream.ChannelLayout {
	case "stereo":
		audioFileNameChannelsPart = "20"
	case "5.1":
		audioFileNameChannelsPart = "51"
	default:
		{
			switch audioStream.Channels {
			case 2:
				audioFileNameChannelsPart = "20"
			case 6:
				audioFileNameChannelsPart = "51"
			}
		}
	}

	return
}

func ProcessMovie(srcPath string, movieLock *sync.Mutex) {
	info, err := ffinfo.Probe(srcPath)
	if err != nil {
		log.Printf("ffprobe start error: %s", err)
		return
	}

	video := GetVideoStream(info.Streams)

	if video == nil {
		log.Println("в файле нет видео")
		return
	}

	if video.Width < 1920 {
		log.Printf("не могу посчитать автоматически, потому что ширина исходника меньше 1920")
		return
	}

	cmd := exec.Command(
		"fflite",
		"-n",
		"-r",
		"25",
		"-i",
		srcPath,
		"-map",
		"0:v:0",
		"@crf16",
		"-vf",
		"scale=1920:-2,pad=1920:1080:-1:-1,setsar=1/1",
		path.Join(viper.GetString("path.edit.cache"),
			strings.TrimSuffix(path.Base(srcPath), path.Ext(srcPath))+"_HD.mp4"),
		"-map",
		"0:a?",
		"-c:a",
		"copy",
		path.Join(viper.GetString("path.buffer.in"),
			strings.TrimSuffix(path.Base(srcPath), path.Ext(srcPath))+"_AUDIO"+path.Ext(srcPath)),
	)

	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout

	movieLock.Lock()
	err = cmd.Run()
	movieLock.Unlock()

	if err != nil {
		log.Printf("cmd.Run() failed with %s\n", err)
	}
}

type TrlUserInput struct {
	name string
}

type UserInputRequestMsgGroup struct {
	statusMsgId    int
	photosGroupIDs []int
}

type tgBot struct {
	channel  tele.ChatID
	tgApi    *tele.Bot
	requests map[string]UserInputRequestMsgGroup
}

func NewTgBot() *tgBot {
	tgApi, err := tele.NewBot(tele.Settings{
		Token: os.Getenv("TOKEN"),
		Poller: &tele.LongPoller{
			Timeout: 10 * time.Second,
		},
	})

	if err != nil {
		log.Panic(err)
	}

	bot := &tgBot{
		channel:  tele.ChatID(-1001675392122),
		tgApi:    tgApi,
		requests: make(map[string]UserInputRequestMsgGroup),
	}

	// go bot.updateGorutine()

	return bot
}

// func (self *tgBot) updateGorutine() {
// 	u := tgbotapi.NewUpdate(0)
// 	u.Timeout = 60

// 	updates, err := self.botApi.GetUpdatesChan(u)

// 	if err != nil {
// 		log.Panic(err)
// 	}

// 	for update := range updates {
// 		if update.ChannelPost == nil {
// 			continue
// 		}

// 		if update.ChannelPost.ReplyToMessage == nil {
// 			continue
// 		}

// 		if _, ok := self.awaitResolution[update.ChannelPost.ReplyToMessage.MessageID]; !ok {
// 			continue
// 		}

// 		self.awaitResolution[update.ChannelPost.ReplyToMessage.MessageID] <- update.ChannelPost.Text

// 		delete(self.awaitResolution, update.ChannelPost.ReplyToMessage.MessageID)

// 		self.botApi.Send(tgbotapi.NewDeleteMessage(
// 			self.channel,
// 			update.ChannelPost.ReplyToMessage.MessageID,
// 		))

// 		self.botApi.Send(tgbotapi.NewDeleteMessage(
// 			self.channel,
// 			update.ChannelPost.MessageID,
// 		))
// 	}
// }

func (self *tgBot) requestTRL(fileName string) {
	self.requests[fileName] = UserInputRequestMsgGroup{
		statusMsgId:    0,
		photosGroupIDs: make([]int, 0),
	}
	userInput := TrlUserInput{}

	requestMsg := fmt.Sprintf("❓ %s", fileName)
	inQueue := fmt.Sprintf("🕐 %s", fileName)
	inMove := fmt.Sprintf("📤 %s", fileName)
	finishMsg := fmt.Sprintf("✅ %s", fileName)

	titleMsg, err := self.tgApi.Send(
		self.channel,
		requestMsg,
	)
	if err != nil {
		log.Panic(err)
	}

	self.requests[fileName].statusMsgId = titleMsg.ID

	// Отправляем 3 группы фотографий
	album := NewScreenshotAlbum(fileName, 5, "0", "10")

	_, err = self.tgApi.SendAlbum(
		self.channel,
		album,
	)

	if err != nil {
		log.Panic(err)
	}

	// self.requests[fileName].photosGroupIDs = append(
	// 	self.requests[fileName].photosGroupIDs,
	// 	albumMsg.ID,
	// )

	// Отправляем всё аудио, которое есть
	// Отправляем панель с действиями
}

func NewScreenshotAlbum(filePath string, count int, ss, to string) tele.Album {
	var album tele.Album

	l, err := net.Listen("tcp", "localhost:8080")
	if err != nil {
		panic(err)
	}
	defer l.Close()

	for i := 1; i <= count; i++ {
		cmd := exec.Command(
			"ffmpeg",
			"-hide_banner",
			"-ss",
			ss,
			"-to",
			to,
			"-i",
			filePath,
			"-map",
			"0:v:0",
			"-vf",
			"scale=iw*sar:ih,setsar=1/1",
			"-frames:v",
			"1",
			"-q:v",
			"8",
			"-f",
			"image2pipe",
			"tcp://localhost:8080",
		)

		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		err = cmd.Run()
		if err != nil {
			log.Fatalf("cmd.Run() failed with %s\n", err)
		}

		c, err := l.Accept()

		if err != nil {
			panic(err)
		}

		album = append(
			album,
			&tele.Photo{File: tele.FromReader(c)},
		)
	}

	return album
}

func ProcessTrl(srcPath string, bot *tgBot, trlLock *sync.Mutex) {
	info, err := ffinfo.Probe(srcPath)
	if err != nil {
		log.Printf("ffprobe вернул ошибку: %s", err)
		return
	}

	videoStream := GetVideoStream(info.Streams)

	if videoStream == nil {
		log.Println("Не найден ни один видео-стрим")
		return
	}

	if videoStream.RFrameRate == "" {
		log.Printf("ffprobe не может предоставить информацию о фреймрейте: RFrameRate пустая строка")
		return
	}

	if videoStream.SampleAspectRatio != "1:1" && videoStream.SampleAspectRatio != "" {
		log.Printf("Trailer sar != 1:1")
		return
	}

	// Тут ещё должна быть добавлена проверка на наличие рамки

	if videoStream.Width < 1920 && videoStream.Height < 1080 {
		log.Printf("Trailer need upscale")
		return
	}

	audioStreams := GetAllAudioStreams(info)

	if !IsAllAudioChannelsMixed(info) {
		if len(audioStreams) == 0 {
			log.Printf("Нет аудио")
		} else {
			log.Printf("Есть аудио стримы кроме 2.0 или 5.1")
		}
	}

	// Там один или несколько 5.1 или 2.0 стримов

	// audioFileNameChannelsPart = GetAudioFileNameChannelsPart(selected)
	audioFileNameChannelsPart := "20"
	tgNameInput := "test"

	// Нужно спросить имя трейлера и какой аудио стрим выбрать
	bot.requestTRL(path.Base(srcPath))
	return

	cmd := exec.Command(
		"fflite",
		"-n",
		"-r",
		"25",
		"-i",
		srcPath,
		"-map",
		"0:v:0",
		"@crf10",
		"-vf",
		"scale='if(gte(dar, 16/9), 1920, -2):if(gte(dar, 16/9), -2, 1080)',setsar=1/1,pad=1920:1080:-1:-1",
		path.Join(viper.GetString("path.edit.trailers_temp"),
			tgNameInput+"_TRL"+"_HD.mp4"),
		"-map",
		"0:a:0",
		"@alac0",
		"-af",
		fmt.Sprintf("aresample=48000,atempo=25/(%s)", videoStream.RFrameRate),
		path.Join(viper.GetString("path.edit.trailers_temp"),
			tgNameInput+"_TRL"+"_AUDIORUS"+audioFileNameChannelsPart+".m4a"),
	)

	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout

	trlLock.Lock()
	err = cmd.Run()
	trlLock.Unlock()

	if err != nil {
		log.Printf("cmd.Run() failed with %s\n", err)
	}

	destPath := path.Join(
		viper.GetString("path.in.trailers"),
		"_DONE",
		tgNameInput+"_TRL",
	)

	yos.MakeDir(destPath)
	yos.MoveFile(srcPath, destPath)
}
