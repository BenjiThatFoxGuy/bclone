#!/usr/bin/env python3
import subprocess, os, sys
root = os.getcwd()
files = subprocess.check_output(["git","ls-files","*.go"], cwd=root).decode().splitlines()
converted = []
failed = []
for f in files:
    path = os.path.join(root, f)
    try:
        data = open(path, 'rb').read()
    except Exception as e:
        failed.append((f, str(e)))
        continue
    if b'\x00' not in data:
        continue
    # try utf-8 first
    try:
        text = data.decode('utf-8')
        # if decoded utf-8 contains NUL characters, treat as failure
        if "\x00" in text:
            raise UnicodeDecodeError('utf-8','',0,1,'contains NUL')
        # already valid utf-8
        continue
    except Exception:
        pass
    dec_success = False
    for enc in ('utf-16','utf-16-le','utf-16-be'):
        try:
            text = data.decode(enc)
            # strip trailing BOM if any
            if text and text[0] == '\ufeff':
                text = text[1:]
            # write back as utf-8
            with open(path, 'w', encoding='utf-8', newline='\n') as w:
                w.write(text)
            converted.append(f)
            dec_success = True
            break
        except Exception:
            continue
    if not dec_success:
        failed.append((f, 'could not decode as utf-16'))

print('Converted files:', len(converted))
for c in converted:
    print(c)
if failed:
    print('\nFailed conversions:', len(failed))
    for f,err in failed:
        print(f, err)
