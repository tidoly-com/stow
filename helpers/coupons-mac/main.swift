// The coupon helper for macOS: reads the words in a picture with the system's own Vision framework, so
// nothing has to be shipped with it — the photo server finds the barcode itself and asks this only for
// the words, where a coupon's last day is. The server starts it and speaks to it over standard input and
// output, one JSON object per line each way:
//
//	-> {"picture": "/path/to/picture"}
//	<- {"lines": ["...", ...]}   or {"error": "..."}
import Foundation
import ImageIO
import Vision

struct Ask: Decodable { let picture: String }
struct Answer: Encodable { var lines: [String] = []; var error: String? = nil }

func picture(_ path: String) -> CGImage? {
    guard let src = CGImageSourceCreateWithURL(URL(fileURLWithPath: path) as CFURL, nil) else { return nil }
    let opts: [CFString: Any] = [kCGImageSourceCreateThumbnailFromImageAlways: true, kCGImageSourceThumbnailMaxPixelSize: 2048,
                                 kCGImageSourceCreateThumbnailWithTransform: true]
    return CGImageSourceCreateThumbnailAtIndex(src, 0, opts as CFDictionary)
}

func answer(_ ask: Ask) -> Answer {
    guard let img = picture(ask.picture) else { return Answer(error: "cannot read the picture") }
    let handler = VNImageRequestHandler(cgImage: img, options: [:])
    var out = Answer()
    let words = VNRecognizeTextRequest()
    words.recognitionLevel = .accurate
    words.recognitionLanguages = ["ko-KR", "en-US"]
    do { try handler.perform([words]) } catch { return Answer(error: "words: \(error.localizedDescription)") }
    out.lines = (words.results ?? []).compactMap { $0.topCandidates(1).first?.string }
    return out
}

let encoder = JSONEncoder()
while let line = readLine() {
    var reply: Answer
    if let ask = try? JSONDecoder().decode(Ask.self, from: Data(line.utf8)) {
        reply = autoreleasepool { answer(ask) }
    } else {
        reply = Answer(error: "cannot read the request")
    }
    FileHandle.standardOutput.write((try? encoder.encode(reply)) ?? Data("{\"error\":\"cannot write the answer\"}".utf8))
    FileHandle.standardOutput.write(Data("\n".utf8))
}
