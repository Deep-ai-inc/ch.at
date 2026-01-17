package main

import (
	"fmt"
	"net/http"
	"strings"
)

// isPoopBaby checks if the query triggers the easter egg and handles it
func isPoopBaby(query string, w http.ResponseWriter) bool {
	if strings.EqualFold(strings.TrimSpace(query), "poopbaby") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, poopBabyHTML)
		return true
	}
	return false
}

const poopBabyHTML = `<!DOCTYPE html>
<html>
<head>
    <title>PoopBaby</title>
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <style>
        * { margin: 0; padding: 0; box-sizing: border-box; }
        body {
            background: linear-gradient(135deg, #ffe4f3 0%, #fff9e6 100%);
            min-height: 100vh;
            display: flex;
            flex-direction: column;
            align-items: center;
            justify-content: center;
            font-family: system-ui, sans-serif;
            cursor: pointer;
            overflow: hidden;
            user-select: none;
        }
        .hint {
            position: fixed;
            top: 20px;
            color: #888;
            font-size: 14px;
        }
        .baby {
            font-size: 150px;
            animation: bounce 0.5s ease-in-out infinite alternate;
            position: relative;
            z-index: 10;
        }
        @keyframes bounce {
            from { transform: translateY(0); }
            to { transform: translateY(-10px); }
        }
        .poop {
            position: absolute;
            font-size: 50px;
            pointer-events: none;
            animation: fall 2s ease-in forwards;
            z-index: 5;
        }
        @keyframes fall {
            0% { opacity: 1; transform: translateY(0) rotate(0deg); }
            100% { opacity: 0; transform: translateY(300px) rotate(360deg); }
        }
        .score {
            position: fixed;
            bottom: 20px;
            font-size: 24px;
            color: #8B4513;
            background: linear-gradient(135deg, #ffe4f3 0%, #fff9e6 100%);
            padding: 10px 20px;
            border-radius: 10px;
            z-index: 100;
        }
        .splat-zone {
            position: fixed;
            bottom: 80px;
            left: 0;
            right: 0;
            height: 60px;
        }
        .splat {
            position: fixed;
            font-size: 30px;
            pointer-events: none;
            animation: splat 0.5s ease-out forwards;
        }
        @keyframes splat {
            0% { transform: scale(0); opacity: 1; }
            50% { transform: scale(1.5); opacity: 1; }
            100% { transform: scale(1); opacity: 0.7; }
        }
    </style>
</head>
<body>
    <div class="hint">Click anywhere! Press ESC to exit</div>
    <div class="baby">&#128118;</div>
    <div class="score">Poops: <span id="count">0</span></div>

    <script>
        let count = 0;
        const baby = document.querySelector('.baby');
        const countEl = document.getElementById('count');
        const poopEmojis = ['&#128169;'];

        document.addEventListener('click', function(e) {
            count++;
            countEl.textContent = count;

            // Create poop from baby
            const poop = document.createElement('div');
            poop.className = 'poop';
            poop.innerHTML = poopEmojis[Math.floor(Math.random() * poopEmojis.length)];

            const babyRect = baby.getBoundingClientRect();
            poop.style.left = (babyRect.left + babyRect.width/2 - 25) + 'px';
            poop.style.top = (babyRect.bottom - 30) + 'px';

            document.body.appendChild(poop);

            // Add splat when poop lands
            setTimeout(function() {
                const splat = document.createElement('div');
                splat.className = 'splat';
                splat.innerHTML = '&#128169;';
                splat.style.left = (babyRect.left + babyRect.width/2 - 15 + (Math.random()-0.5)*100) + 'px';
                splat.style.bottom = '100px';
                document.body.appendChild(splat);
            }, 1800);

            // Cleanup old elements
            setTimeout(function() { poop.remove(); }, 2100);
        });

        window.addEventListener('keydown', function(e) {
            if (e.key === 'Escape') {
                e.preventDefault();
                window.location.href = '/';
            }
        });
    </script>
</body>
</html>`
