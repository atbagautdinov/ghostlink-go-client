#ifndef GHOSTLINK_CURSES_H
#define GHOSTLINK_CURSES_H

#ifdef __cplusplus
extern "C" {
#endif

int cursesInit(void);
void cursesShutdown(void);
void cursesResize(void);
void cursesPrint(const char *text, int color);
void cursesClear(void);
void cursesHeader(const char *text);
int cursesPrompt(const char *prompt, char *buf, int len);
int cursesKey(int ms);
void cursesDrawInput(const char *prompt, const char *input);
int cursesCols(void);
int cursesChatRows(void);

#ifdef __cplusplus
}
#endif

#endif /* GHOSTLINK_CURSES_H */