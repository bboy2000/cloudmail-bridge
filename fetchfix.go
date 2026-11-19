package main

// IMAP FETCH 的 BODY[] section 正确切片 + BODYSTRUCTURE 构造。
// 背景：早期 Fetch 实现不论客户端请求哪个 section 都返回整封 raw，
// 导致网易邮箱大师等严格客户端在 BODY.PEEK[HEADER] 收到"头+正文"后
// 协议层校验失败、静默丢弃邮件（能连上、能拉、界面空白）。

import (
	"bytes"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"

	"github.com/emersion/go-imap/v2"
)

// splitHeaderBody 把 RFC822 原文拆成"头部(含结尾空行)"和"正文"。
func splitHeaderBody(raw []byte) (header, body []byte) {
	idx := bytes.Index(raw, []byte("\r\n\r\n"))
	if idx >= 0 {
		return raw[:idx+4], raw[idx+4:]
	}
	// 兼容只用 LF 的邮件
	idx = bytes.Index(raw, []byte("\n\n"))
	if idx >= 0 {
		return raw[:idx+2], raw[idx+2:]
	}
	return raw, nil
}

// headerValue 取指定头字段的值（不含字段名），大小写不敏感。
func headerValue(header []byte, name string) string {
	msg, err := mail.ReadMessage(bytes.NewReader(append(append([]byte{}, header...), []byte("\r\n")...)))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(msg.Header.Get(name))
}

// sectionBytes 按 IMAP BODY[section] 的语义从 raw 中取出对应字节。
// 返回的字节数必须与实际内容一致（调用方据此写 literal 长度）。
func sectionBytes(raw []byte, bs *imap.FetchItemBodySection) []byte {
	if bs == nil {
		return raw
	}

	header, body := splitHeaderBody(raw)

	// BODY[] / BODY.PEEK[]：整封（含头）
	if len(bs.Part) == 0 && bs.Specifier == imap.PartSpecifierNone {
		return raw
	}

	// 顶层 HEADER 系列（无 part 前缀）
	if len(bs.Part) == 0 {
		switch bs.Specifier {
		case imap.PartSpecifierHeader:
			if len(bs.HeaderFields) == 0 && len(bs.HeaderFieldsNot) == 0 {
				return header
			}
			return filterHeaderFields(header, bs.HeaderFields, bs.HeaderFieldsNot)
		case imap.PartSpecifierText:
			return body
		case imap.PartSpecifierMIME:
			return header
		}
	}

	// 带 part 前缀：解析 multipart，按路径取分片
	part := partBytes(raw, bs.Part)
	if part == nil {
		return nil
	}
	ph, pb := splitHeaderBody(part)
	switch bs.Specifier {
	case imap.PartSpecifierNone:
		// RFC3501: BODY[n] = "the body of that part"，即该 part 的正文，
		// 不含它自己的 MIME 头；part 的 MIME 头由 BODY[n.MIME] 单独提供。
		// 若把头+正文一起返回，客户端按标准当纯正文处理，会把
		// Content-Type / Content-Transfer-Encoding 也显示出来。
		return pb
	case imap.PartSpecifierHeader:
		if len(bs.HeaderFields) == 0 && len(bs.HeaderFieldsNot) == 0 {
			return ph
		}
		return filterHeaderFields(ph, bs.HeaderFields, bs.HeaderFieldsNot)
	case imap.PartSpecifierText:
		return pb
	case imap.PartSpecifierMIME:
		return ph
	}
	return pb
}

// filterHeaderFields 按 BODY[HEADER.FIELDS / HEADER.FIELDS.NOT] 过滤头字段。
// 结果以空行结束（RFC3501 要求 literal 内含结尾 CRLF CRLF）。
func filterHeaderFields(header []byte, include, exclude []string) []byte {
	lines := bytes.Split(header, []byte("\r\n"))
	inSet := map[string]bool{}
	for _, f := range include {
		inSet[strings.ToLower(f)] = true
	}
	exSet := map[string]bool{}
	for _, f := range exclude {
		exSet[strings.ToLower(f)] = true
	}

	var out bytes.Buffer
	keep := false
	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		// 续行（以空格/TAB 开头）跟随上一字段的取舍
		if line[0] == ' ' || line[0] == '\t' {
			if keep {
				out.Write(line)
				out.WriteString("\r\n")
			}
			continue
		}
		colon := bytes.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name := strings.ToLower(string(line[:colon]))
		if len(inSet) > 0 {
			keep = inSet[name]
		} else if len(exSet) > 0 {
			keep = !exSet[name]
		} else {
			keep = false
		}
		if keep {
			out.Write(line)
			out.WriteString("\r\n")
		}
	}
	out.WriteString("\r\n")
	return out.Bytes()
}

// partBytes 按 part 路径（如 [1]、[1,2]）取 MIME 子分片原文。
func partBytes(raw []byte, path []int) []byte {
	if len(path) == 0 {
		return raw
	}
	header, body := splitHeaderBody(raw)
	ct := headerValue(header, "Content-Type")
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil || !strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		// 非 multipart 时，[1] 通常指整个 body
		if len(path) == 1 && path[0] == 1 {
			return raw
		}
		return nil
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	want := path[0]
	for i := 1; ; i++ {
		p, err := mr.NextPart()
		if err != nil {
			return nil
		}
		data, _ := readAll(p)
		// multipart.Reader 只给正文，按 part 头重建原文（头+正文）
		var b bytes.Buffer
		for k, vals := range p.Header {
			for _, v := range vals {
				b.WriteString(k + ": " + v + "\r\n")
			}
		}
		b.WriteString("\r\n")
		b.Write(data)
		if i == want {
			if len(path) == 1 {
				return b.Bytes()
			}
			return partBytes(b.Bytes(), path[1:])
		}
	}
}

func readAll(p *multipart.Part) ([]byte, error) {
	var b bytes.Buffer
	_, err := b.ReadFrom(p)
	return b.Bytes(), err
}

// bodyStructureOf 递归解析整封 RFC822，构造准确的 BODYSTRUCTURE。
// 两个关键点（否则客户端附件展示会出错）：
//  1. multipart 必须递归成 BodyStructureMultiPart，不能再把内层容器
//     （如正文的 multipart/alternative）当成 single part 报出去——
//     否则客户端会把这个容器当成一个"未命名附件"。
//  2. 附件 part 必须带 Content-Disposition: attachment + filename，
//     客户端据此识别附件并取文件名；缺失会导致有的客户端看不到附件。
func bodyStructureOf(raw []byte) imap.BodyStructure {
	header, body := splitHeaderBody(raw)
	ct := headerValue(header, "Content-Type")
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil || strings.TrimSpace(ct) == "" {
		mediaType = "text/plain"
		params = nil
	}
	return partStructure(mediaType, params, header, body)
}

// partStructure 解析单个 MIME part；遇到 multipart/* 递归解析子 part。
// partHeader 是该 part 自己的 MIME 头字节（含结尾空行），用于取 CTE / Disposition。
func partStructure(mediaType string, params map[string]string, partHeader, body []byte) imap.BodyStructure {
	if strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		subtype := "mixed"
		if i := strings.IndexByte(mediaType, '/'); i >= 0 {
			subtype = mediaType[i+1:]
		}
		boundary := params["boundary"]
		children := []imap.BodyStructure{}
		if boundary != "" {
			mr := multipart.NewReader(bytes.NewReader(body), boundary)
			for {
				p, err := mr.NextPart()
				if err != nil {
					break
				}
				data, _ := readAll(p)
				childCT := p.Header.Get("Content-Type")
				childType, childParams, e := mime.ParseMediaType(childCT)
				if e != nil {
					childType, childParams = "text/plain", map[string]string{"charset": "utf-8"}
				}
				children = append(children, partStructure(childType, childParams, partHeaderBytes(p.Header), data))
			}
		}
		if len(children) == 0 {
			children = []imap.BodyStructure{
				&imap.BodyStructureSinglePart{Type: "text", Subtype: "plain", Size: 0, Encoding: "7bit", Extended: &imap.BodyStructureSinglePartExt{}},
			}
		}
		return &imap.BodyStructureMultiPart{
			Children: children,
			Subtype:  subtype,
			Extended: &imap.BodyStructureMultiPartExt{Params: params},
		}
	}
	return singlePartStructure(mediaType, params, headerValue(partHeader, "Content-Transfer-Encoding"), body, partHeader)
}

// partHeaderBytes 把 multipart.Part 的 MIME 头重建为"头+结尾空行"字节，
// 供 headerValue 复用。header key 用 textproto 的规范形式写出。
func partHeaderBytes(h map[string][]string) []byte {
	var b bytes.Buffer
	for k, vals := range h {
		for _, v := range vals {
			b.WriteString(k + ": " + v + "\r\n")
		}
	}
	b.WriteString("\r\n")
	return b.Bytes()
}

func singlePartStructure(mediaType string, params map[string]string, encoding string, body []byte, partHeader []byte) *imap.BodyStructureSinglePart {
	typ, subtype := "text", "plain"
	if mediaType != "" {
		if i := strings.IndexByte(mediaType, '/'); i >= 0 {
			typ, subtype = mediaType[:i], mediaType[i+1:]
		} else {
			typ = mediaType
		}
	}
	if params == nil {
		params = map[string]string{}
	}
	if encoding == "" {
		encoding = "7bit"
	}
	ext := &imap.BodyStructureSinglePartExt{}
	// 关键：从 Content-Disposition 解析出 attachment + filename 并带进
	// BODYSTRUCTURE。go-imap 的 Filename() 优先读 Disposition.Params["filename"]，
	// 客户端（网易邮箱大师等）据此识别附件、显示文件名。
	if disp := headerValue(partHeader, "Content-Disposition"); disp != "" {
		if dv, dparams, e := mime.ParseMediaType(disp); e == nil && dv != "" {
			ext.Disposition = &imap.BodyStructureDisposition{Value: dv, Params: dparams}
		}
	}
	bs := &imap.BodyStructureSinglePart{
		Type:     typ,
		Subtype:  subtype,
		Params:   params,
		Encoding: encoding,
		Size:     uint32(len(body)),
		Extended: ext,
	}
	if strings.EqualFold(typ, "text") {
		bs.Text = &imap.BodyStructureText{NumLines: int64(bytes.Count(body, []byte("\n")))}
	}
	return bs
}
