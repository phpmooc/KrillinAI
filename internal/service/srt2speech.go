package service

import (
	"context"
	"fmt"
	"krillin-ai/internal/storage"
	"krillin-ai/internal/types"
	"krillin-ai/log"
	"krillin-ai/pkg/util"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// 输入中文字幕，生成配音
func (s Service) srtFileToSpeech(ctx context.Context, stepParam *types.SubtitleTaskStepParam) error {
	if !stepParam.EnableTts {
		return nil
	}
	// Step 1: 解析字幕文件
	subtitles, err := parseSRT(stepParam.TtsSourceFilePath)
	if err != nil {
		log.GetLogger().Error("srtFileToSpeech parseSRT error", zap.Any("stepParam", stepParam), zap.Error(err))
		return fmt.Errorf("srtFileToSpeech parseSRT error: %w", err)
	}

	var audioFiles []string
	var currentTime time.Time

	// 创建文件记录音频的开始和结束时间
	durationDetailFile, err := os.Create(filepath.Join(stepParam.TaskBasePath, types.TtsAudioDurationDetailsFileName))
	if err != nil {
		log.GetLogger().Error("srtFileToSpeech create durationDetailFile error", zap.Any("stepParam", stepParam), zap.Error(err))
		return fmt.Errorf("srtFileToSpeech create durationDetailFile error: %w", err)
	}
	defer durationDetailFile.Close()

	// Step 2: 使用 阿里云TTS
	// 判断是否使用音色克隆
	voiceCode := stepParam.TtsVoiceCode
	if stepParam.VoiceCloneAudioUrl != "" {
		var code string
		code, err = s.VoiceCloneClient.CosyVoiceClone("krillinai", stepParam.VoiceCloneAudioUrl)
		if err != nil {
			log.GetLogger().Error("srtFileToSpeech CosyVoiceClone error", zap.Any("stepParam", stepParam), zap.Error(err))
			return fmt.Errorf("srtFileToSpeech CosyVoiceClone error: %w", err)
		}
		voiceCode = code
	}

	// 并发处理TTS转换
	err = s.processSubtitlesConcurrently(subtitles, voiceCode, stepParam)
	if err != nil {
		log.GetLogger().Error("srtFileToSpeech processSubtitlesConcurrently error", zap.Any("stepParam", stepParam), zap.Error(err))
		return fmt.Errorf("srtFileToSpeech processSubtitlesConcurrently error: %w", err)
	}

	for i, sub := range subtitles {
		outputFile := filepath.Join(stepParam.TaskBasePath, fmt.Sprintf("subtitle_%d.wav", i+1))

		// Step 3: 调整音频时长
		startTime, err := time.Parse("15:04:05,000", sub.Start)
		if err != nil {
			log.GetLogger().Error("srtFileToSpeech parse time error", zap.Any("stepParam", stepParam), zap.Any("num", i+1), zap.String("time str", sub.Start), zap.Error(err))
			return fmt.Errorf("srtFileToSpeech parse time error: %w", err)
		}
		endTime, err := time.Parse("15:04:05,000", sub.End)
		if err != nil {
			log.GetLogger().Error("audioToSubtitle.time.Parse error", zap.Any("stepParam", stepParam), zap.Any("num", i+1), zap.String("time str", sub.Start), zap.Error(err))
			return fmt.Errorf("srtFileToSpeech audioToSubtitle.time.Parse error: %w", err)
		}
		if i == 0 {
			// 如果第一条字幕不是从00:00开始，增加静音帧
			if startTime.Second() > 0 {
				silenceDurationMs := startTime.Sub(time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)).Milliseconds()
				silenceFilePath := filepath.Join(stepParam.TaskBasePath, "silence_0.wav")
				err := newGenerateSilence(silenceFilePath, float64(silenceDurationMs)/1000)
				if err != nil {
					log.GetLogger().Error("srtFileToSpeech newGenerateSilence error", zap.Any("stepParam", stepParam), zap.Error(err))
					return fmt.Errorf("srtFileToSpeech newGenerateSilence error: %w", err)
				}
				audioFiles = append(audioFiles, silenceFilePath)

				// 计算静音帧的结束时间
				silenceEndTime := currentTime.Add(time.Duration(silenceDurationMs) * time.Millisecond)
				durationDetailFile.WriteString(fmt.Sprintf("Silence: start=%s, end=%s\n", currentTime.Format("15:04:05,000"), silenceEndTime.Format("15:04:05,000")))
				currentTime = silenceEndTime
			}
		}

		duration := endTime.Sub(startTime).Seconds()
		if i < len(subtitles)-1 {
			// 如果不是最后一条字幕，增加静音帧时长
			nextStartTime, err := time.Parse("15:04:05,000", subtitles[i+1].Start)
			if err != nil {
				log.GetLogger().Error("srtFileToSpeech parse time error", zap.Any("stepParam", stepParam), zap.Any("num", i+2), zap.String("time str", subtitles[i+1].Start), zap.Error(err))
				return fmt.Errorf("srtFileToSpeech parse time error: %w", err)
			}
			if endTime.Before(nextStartTime) {
				duration = nextStartTime.Sub(startTime).Seconds()
			}
		}

		adjustedFile := filepath.Join(stepParam.TaskBasePath, fmt.Sprintf("adjusted_%d.wav", i+1))
		err = adjustAudioDuration(outputFile, adjustedFile, stepParam.TaskBasePath, duration)
		if err != nil {
			log.GetLogger().Error("srtFileToSpeech adjustAudioDuration error", zap.Any("stepParam", stepParam), zap.Any("num", i+1), zap.Error(err))
			return fmt.Errorf("srtFileToSpeech adjustAudioDuration error: %w", err)
		}

		audioFiles = append(audioFiles, adjustedFile)

		// 计算音频的实际时长
		audioDuration, err := util.GetAudioDuration(adjustedFile)
		if err != nil {
			log.GetLogger().Error("srtFileToSpeech GetAudioDuration error", zap.Any("stepParam", stepParam), zap.Any("num", i+1), zap.Error(err))
			return fmt.Errorf("srtFileToSpeech GetAudioDuration error: %w", err)
		}

		// 计算音频的结束时间
		audioEndTime := currentTime.Add(time.Duration(audioDuration*1000) * time.Millisecond)
		// 写入文件
		durationDetailFile.WriteString(fmt.Sprintf("Audio %d: start=%s, end=%s\n", i+1, currentTime.Format("15:04:05,000"), audioEndTime.Format("15:04:05,000")))
		currentTime = audioEndTime
	}

	// Step 6: 拼接所有音频文件
	finalOutput := filepath.Join(stepParam.TaskBasePath, types.TtsResultAudioFileName)
	err = concatenateAudioFiles(audioFiles, finalOutput, stepParam.TaskBasePath)
	if err != nil {
		log.GetLogger().Error("srtFileToSpeech concatenateAudioFiles error", zap.Any("stepParam", stepParam), zap.Error(err))
		return fmt.Errorf("srtFileToSpeech concatenateAudioFiles error: %w", err)
	}
	stepParam.TtsResultFilePath = finalOutput

	videoWithTtsPath := filepath.Join(stepParam.TaskBasePath, types.SubtitleTaskVideoWithTtsFileName)
	// 合成音频替换后的新视频
	err = util.ReplaceAudioInVideo(stepParam.InputVideoPath, finalOutput, videoWithTtsPath)
	if err != nil {
		log.GetLogger().Error("srtFileToSpeech ReplaceAudioInVideo error", zap.Any("stepParam", stepParam), zap.Error(err))
	}
	stepParam.VideoWithTtsFilePath = videoWithTtsPath
	// 更新字幕任务信息
	stepParam.TaskPtr.ProcessPct = 98
	log.GetLogger().Info("srtFileToSpeech success", zap.String("task id", stepParam.TaskId))
	return nil
}

func (s Service) processSubtitlesConcurrently(subtitles []types.SrtSentenceWithStrTime, voiceCode string, stepParam *types.SubtitleTaskStepParam) error {
	// 创建一个结果数组来存储每个字幕的处理结果
	type processingResult struct {
		index int
		err   error
	}

	maxConcurrency := 3 // 降低并发数以减少网络压力
	semaphore := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	resultCh := make(chan processingResult, len(subtitles))

	// 并发生成所有音频文件
	for i, sub := range subtitles {
		wg.Add(1)
		go func(index int, subtitle types.SrtSentenceWithStrTime) {
			defer wg.Done()

			// 获取信号量
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			outputFile := filepath.Join(stepParam.TaskBasePath, fmt.Sprintf("subtitle_%d.wav", index+1))
			err := s.TtsClient.Text2Speech(subtitle.Text, voiceCode, outputFile)
			if err != nil {
				log.GetLogger().Error("processSubtitlesConcurrently Text2Speech error",
					zap.Any("index", index+1),
					zap.String("text", subtitle.Text),
					zap.Error(err))
				resultCh <- processingResult{index: index, err: fmt.Errorf("subtitle %d TTS error: %w", index+1, err)}
				return
			}

			// 成功处理
			resultCh <- processingResult{index: index, err: nil}
		}(i, sub)
	}

	// 等待所有goroutine完成
	wg.Wait()
	close(resultCh)

	// 收集所有结果并统计错误
	results := make([]processingResult, len(subtitles))
	errorCount := 0
	var firstError error

	for result := range resultCh {
		results[result.index] = result
		if result.err != nil {
			errorCount++
			if firstError == nil {
				firstError = result.err
			}
		}
	}

	// 如果有超过一半的字幕失败，则返回错误
	failureThreshold := len(subtitles) / 2
	if errorCount > failureThreshold {
		log.GetLogger().Error("processSubtitlesConcurrently: too many failures",
			zap.Int("total", len(subtitles)),
			zap.Int("errors", errorCount),
			zap.Int("threshold", failureThreshold))
		return fmt.Errorf("too many TTS failures: %d/%d failed, first error: %w", errorCount, len(subtitles), firstError)
	}

	// 验证成功的文件是否存在，对于失败的文件生成静音
	for i, result := range results {
		outputFile := filepath.Join(stepParam.TaskBasePath, fmt.Sprintf("subtitle_%d.wav", i+1))

		if result.err != nil {
			// 为失败的字幕生成静音文件
			log.GetLogger().Warn("生成静音文件替代失败的TTS",
				zap.Int("index", i+1),
				zap.String("file", outputFile))

			// 生成0.5秒的静音作为替代
			err := newGenerateSilence(outputFile, 0.5)
			if err != nil {
				log.GetLogger().Error("生成替代静音文件失败",
					zap.Int("index", i+1),
					zap.Error(err))
				return fmt.Errorf("failed to generate silence for subtitle %d: %w", i+1, err)
			}
		} else {
			// 验证成功生成的文件是否存在
			if _, err := os.Stat(outputFile); os.IsNotExist(err) {
				log.GetLogger().Error("processSubtitlesConcurrently output file not exist",
					zap.Any("index", i+1),
					zap.String("file", outputFile))
				return fmt.Errorf("subtitle %d output file not exist: %s", i+1, outputFile)
			}
		}
	}

	if errorCount > 0 {
		log.GetLogger().Warn("processSubtitlesConcurrently completed with some failures",
			zap.Int("total", len(subtitles)),
			zap.Int("errors", errorCount),
			zap.Int("success", len(subtitles)-errorCount))
	} else {
		log.GetLogger().Info("processSubtitlesConcurrently completed successfully", zap.Int("total", len(subtitles)))
	}

	return nil
}

func parseSRT(filePath string) ([]types.SrtSentenceWithStrTime, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("parseSRT read file error: %w", err)
	}

	var subtitles []types.SrtSentenceWithStrTime
	re := regexp.MustCompile(`(\d{2}:\d{2}:\d{2},\d{3}) --> (\d{2}:\d{2}:\d{2},\d{3})\s+(.+?)\n`)
	matches := re.FindAllStringSubmatch(string(data), -1)

	for _, match := range matches {
		subtitles = append(subtitles, types.SrtSentenceWithStrTime{
			Start: match[1],
			End:   match[2],
			Text:  strings.Replace(match[3], "\n", " ", -1), // 去除换行
		})
	}

	return subtitles, nil
}

func newGenerateSilence(outputAudio string, duration float64) error {
	// 生成 PCM 格式的静音文件
	cmd := exec.Command(storage.FfmpegPath, "-y", "-f", "lavfi", "-i", "anullsrc=channel_layout=mono:sample_rate=44100", "-t",
		fmt.Sprintf("%.3f", duration), "-ar", "44100", "-ac", "1", "-c:a", "pcm_s16le", outputAudio)
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("newGenerateSilence failed to generate PCM silence: %w", err)
	}

	return nil
}

// 调整音频时长，确保音频与字幕时长一致
func adjustAudioDuration(inputFile, outputFile, taskBasePath string, subtitleDuration float64) error {
	// 获取音频时长
	audioDuration, err := util.GetAudioDuration(inputFile)
	if err != nil {
		return err
	}

	// 如果音频时长短于字幕时长，插入静音延长音频
	if audioDuration < subtitleDuration {
		// 计算需要插入的静音时长
		silenceDuration := subtitleDuration - audioDuration

		// 生成静音音频
		silenceFile := filepath.Join(taskBasePath, "silence.wav")
		err := newGenerateSilence(silenceFile, silenceDuration)
		if err != nil {
			return fmt.Errorf("error generating silence: %v", err)
		}

		silenceAudioDuration, _ := util.GetAudioDuration(silenceFile)
		log.GetLogger().Info("adjustAudioDuration", zap.Any("silenceDuration", silenceAudioDuration))

		// 拼接音频和静音
		concatFile := filepath.Join(taskBasePath, "concat.txt")
		f, err := os.Create(concatFile)
		if err != nil {
			return fmt.Errorf("adjustAudioDuration create concat file error: %w", err)
		}
		defer os.Remove(concatFile)

		_, err = f.WriteString(fmt.Sprintf("file '%s'\nfile '%s'\n", filepath.Base(inputFile), filepath.Base(silenceFile)))
		if err != nil {
			return fmt.Errorf("adjustAudioDuration write to concat file error: %v", err)
		}
		f.Close()

		cmd := exec.Command(storage.FfmpegPath, "-y", "-f", "concat", "-safe", "0", "-i", concatFile, "-c", "copy", outputFile)
		log.GetLogger().Info("adjustAudioDuration", zap.Any("inputFile", inputFile), zap.Any("outputFile", outputFile), zap.String("run command", cmd.String()))
		cmd.Stderr = os.Stderr
		err = cmd.Run()
		if err != nil {
			return fmt.Errorf("adjustAudioDuration concat audio and silence  error: %v", err)
		}

		concatFileDuration, _ := util.GetAudioDuration(outputFile)
		log.GetLogger().Info("adjustAudioDuration", zap.Any("concatFileDuration", concatFileDuration))
		return nil
	}

	// 如果音频时长长于字幕时长，缩放音频的播放速率
	if audioDuration > subtitleDuration {
		// 计算播放速率
		speed := audioDuration / subtitleDuration
		//if speed < 0.5 || speed > 2.0 {
		//	// 速率在 FFmpeg 支持的范围内一般是 [0.5, 2.0]
		//	return fmt.Errorf("speed factor %.2f is out of range (0.5 to 2.0)", speed)
		//}

		// 使用 atempo 滤镜调整音频播放速率
		cmd := exec.Command(storage.FfmpegPath, "-y", "-i", inputFile, "-filter:a", fmt.Sprintf("atempo=%.2f", speed), outputFile)
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	// 如果音频时长和字幕时长相同，则直接复制文件
	return util.CopyFile(inputFile, outputFile)
}

// 拼接音频文件
func concatenateAudioFiles(audioFiles []string, outputFile, taskBasePath string) error {
	// 创建一个临时文件保存音频文件列表
	listFile := filepath.Join(taskBasePath, "audio_list.txt")
	f, err := os.Create(listFile)
	if err != nil {
		return err
	}
	defer os.Remove(listFile)

	for _, file := range audioFiles {
		_, err := f.WriteString(fmt.Sprintf("file '%s'\n", filepath.Base(file)))
		if err != nil {
			return err
		}
	}
	f.Close()

	cmd := exec.Command(storage.FfmpegPath, "-y", "-f", "concat", "-safe", "0", "-i", listFile, "-c", "copy", outputFile)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}