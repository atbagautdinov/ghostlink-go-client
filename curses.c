#include "curses.h"
#include <ncurses.h>
#include <stdlib.h>
#include <string.h>

static WINDOW *chatwin = NULL;
static WINDOW *inwin   = NULL;
static int rows = 24, cols = 80;
static char inputbuf[512];

static void redrawInput(const char *prompt) {
    werase(inwin);
    wattron(inwin, COLOR_PAIR(3) | A_BOLD);
    waddstr(inwin, prompt);
    wattroff(inwin, COLOR_PAIR(3) | A_BOLD);
    waddstr(inwin, inputbuf);
    wrefresh(inwin);
}

int cursesInit(void) {
    if (initscr() == NULL) return -1;
    if (has_colors()) {
        start_color();
        use_default_colors();
        init_pair(1, COLOR_GREEN, -1);   /* me */
        init_pair(2, COLOR_CYAN, -1);    /* phone */
        init_pair(3, COLOR_YELLOW, -1);  /* system / prompt */
        init_pair(4, COLOR_WHITE, COLOR_BLUE); /* header */
    }
    cbreak();
    noecho();
    keypad(stdscr, TRUE);
    curs_set(0);
    getmaxyx(stdscr, rows, cols);
    chatwin = newwin(rows - 3, cols, 1, 0);
    inwin   = newwin(2, cols, rows - 2, 0);
    scrollok(chatwin, TRUE);
    return 0;
}

void cursesShutdown(void) {
    if (chatwin) { delwin(chatwin); chatwin = NULL; }
    if (inwin)   { delwin(inwin);   inwin   = NULL; }
    endwin();
}

void cursesResize(void) {
    if (!chatwin || !inwin) return;
    getmaxyx(stdscr, rows, cols);
    wresize(chatwin, rows - 3, cols);
    wresize(inwin, 2, cols);
    mvwin(inwin, rows - 2, 0);
    clear();
    refresh();
}

void cursesPrint(const char *text, int color) {
    if (!chatwin) return;
    wattron(chatwin, COLOR_PAIR(color));
    waddstr(chatwin, text);
    wattroff(chatwin, COLOR_PAIR(color));
    waddstr(chatwin, "\n");
    wrefresh(chatwin);
}

void cursesClear(void) {
    if (!chatwin) return;
    werase(chatwin);
    wrefresh(chatwin);
}

void cursesHeader(const char *text) {
    attron(COLOR_PAIR(4) | A_BOLD);
    mvwhline(stdscr, 0, 0, ' ', cols);
    mvwprintw(stdscr, 0, 1, "%s", text);
    attroff(COLOR_PAIR(4) | A_BOLD);
    refresh();
}

int cursesCols(void) { return cols; }
int cursesChatRows(void) { return rows - 3; }

int cursesPrompt(const char *prompt, char *buf, int len) {
    if (!inwin) return -1;
    werase(inwin);
    wattron(inwin, COLOR_PAIR(3) | A_BOLD);
    waddstr(inwin, prompt);
    wattroff(inwin, COLOR_PAIR(3) | A_BOLD);
    wrefresh(inwin);

    int c = ERR, n = 0;
    wtimeout(inwin, 100); /* poll every 100ms */
    echo();
    curs_set(1);
    while ((c = wgetch(inwin)) != ERR) {
        if (c == 10 || c == 13) { buf[n] = 0; break; }
        if (c == 27 || c == 3) { break; }           /* ESC / Ctrl-C */
        if (c == KEY_BACKSPACE || c == 127 || c == 8) {
            if (n > 0) { n--; inputbuf[n] = 0; }
        } else if (n < len - 1 && c >= 32 && c < 127) {
            inputbuf[n++] = (char)c;
            inputbuf[n] = 0;
        }
        redrawInput(prompt);
    }
    wtimeout(inwin, -1);
    noecho();
    curs_set(0);
    buf[n] = 0;
    return (c == 10 || c == 13) ? 0 : -1;
}