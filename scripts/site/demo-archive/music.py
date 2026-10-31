# A few short original tracks, synthesized here (CC0).
import os, subprocess, sys, wave
import numpy as np
SR = 44100; out = os.path.join(sys.argv[1], 'music', 'synth'); os.makedirs(out, exist_ok=True)
def note(m): return 440 * 2 ** ((m - 69) / 12)
def env(n, a=.01, r=.3):
    t = np.arange(n) / SR; e = np.minimum(1, t / a) * np.exp(-t / r); return e
def tone(f, dur, kind='sine', a=.01, r=.3):
    n = int(dur * SR); t = np.arange(n) / SR; ph = 2 * np.pi * f * t
    w = {'sine': np.sin(ph), 'square': np.sign(np.sin(ph)) * .5, 'saw': 2 * (f * t % 1) - 1, 'tri': 2 * np.abs(2 * (f * t % 1) - 1) - 1}[kind]
    return w * env(n, a, r)
def mix(track, at, s):
    i = int(at * SR); track[i:i + len(s)] += s[:max(0, len(track) - i)]
def render(name, secs, build, fmt):
    l = np.zeros(int(secs * SR)); build(l)
    l = np.tanh(l / max(1e-9, np.abs(l).max()) * 1.4) * .85
    rev = l.copy()
    for d, g in [(.031, .3), (.047, .25), (.073, .2), (.113, .15)]: rev[int(d * SR):] += g * l[:-int(d * SR)]
    st = np.stack([l * .8 + rev * .2, l * .7 + np.roll(rev, 300) * .3], 1); st /= np.abs(st).max() * 1.05
    wav = os.path.join(out, name + '.wav')
    with wave.open(wav, 'wb') as w:
        w.setnchannels(2); w.setsampwidth(2); w.setframerate(SR); w.writeframes((st * 32767).astype('<i2').tobytes())
    if fmt != 'wav':
        args = {'mp3': ['-b:a', '192k'], 'ogg': ['-c:a', 'libopus', '-b:a', '160k'], 'flac': [], 'm4a': ['-c:a', 'aac', '-b:a', '192k']}[fmt]
        subprocess.run(['ffmpeg', '-loglevel', 'error', '-y', '-i', wav, *args, '-metadata', f'title={name.split("-", 1)[1].replace("-", " ").title()}',
                        '-metadata', 'artist=ipfs2fil demo', '-metadata', 'album=Pieces', '-metadata', 'copyright=CC0', os.path.join(out, name + '.' + fmt)], check=True)
        os.remove(wav)
bpm = 96; b = 60 / bpm
def arps(t):
    prog = [[57, 60, 64, 67], [53, 57, 60, 64], [48, 52, 55, 59], [55, 59, 62, 65]]
    for bar in range(24):
        ch = prog[bar % 4]
        for i in range(16): mix(t, bar * 4 * b + i * b / 4, .25 * tone(note(ch[(i * 3) % 4] + 12 * (i // 8)), .5, 'tri', r=.18))
        for m in ch: mix(t, bar * 4 * b, .12 * tone(note(m - 12), 4 * b, 'saw', a=.4, r=2))
        mix(t, bar * 4 * b, .5 * tone(note(ch[0] - 24), 2 * b, 'sine', r=.8))
def pad(t):
    r = np.random.default_rng(3)
    for k in range(40):
        m = int(r.choice([45, 48, 50, 52, 55, 57, 60, 62, 64, 67, 69]))
        mix(t, k * 3.1, .2 * tone(note(m), 9, 'sine', a=2.5, r=3) + .05 * tone(note(m) * 2.005, 9, 'tri', a=3, r=2))
def chip(t):
    mel = [72, 74, 76, 79, 76, 74, 72, 67, 69, 72, 74, 72, 69, 67, 64, 67]
    for bar in range(20):
        for i, m in enumerate(mel): mix(t, bar * 4 * b * .75 + i * b * .75 / 4, .2 * tone(note(m + (bar // 4 % 2) * 5), .14, 'square', r=.06))
        for i in range(4):
            mix(t, (bar * 4 + i) * b * .75, .6 * tone(60 * np.exp(-0) , .25, 'sine', r=.06))
            mix(t, (bar * 4 + i + .5) * b * .75, .15 * np.random.default_rng(i).uniform(-1, 1, int(.05 * SR)) * env(int(.05 * SR), .001, .02))
def drift(t):
    for bar in range(18):
        root = [50, 46, 43, 45][bar % 4]
        for i in range(8): mix(t, bar * 8 * b / 2 + i * b / 2, .3 * tone(note(root + [0, 7, 12, 7, 10, 7, 12, 14][i]), .4, 'saw', r=.12))
        mix(t, bar * 4 * b, .4 * tone(note(root - 12), 4 * b, 'sine', a=.05, r=1.2))
render('01-pieces-in-motion', 24 * 4 * b + 2, arps, 'mp3')
render('02-slow-proofs', 130, pad, 'ogg')  # Opus in Ogg: Homebrew's ffmpeg has no libvorbis
render('03-content-addressed', 20 * 4 * b * .75 + 1, chip, 'flac')
render('04-data-set-drift', 18 * 4 * b + 2, drift, 'm4a')
render('05-short-chime', 4, lambda t: [mix(t, i * .15, .3 * tone(note(m), 2, 'sine', r=.8)) for i, m in enumerate([72, 76, 79, 84])], 'wav')
print('ok')
